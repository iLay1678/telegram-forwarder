// telegram-forwarder 直接使用 Python 版已有挂载、数据库与会话。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/ai"
	"github.com/iLay1678/telegram-forwarder/internal/bot"
	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/logging"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/push"
	"github.com/iLay1678/telegram-forwarder/internal/pyregex"
	"github.com/iLay1678/telegram-forwarder/internal/rss"
	"github.com/iLay1678/telegram-forwarder/internal/rules"
	"github.com/iLay1678/telegram-forwarder/internal/scheduler"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
	"github.com/iLay1678/telegram-forwarder/internal/telegram"
	"github.com/iLay1678/telegram-forwarder/internal/ufb"
	"golang.org/x/sync/errgroup"
)

var version = "dev"
var commit = "unknown"

func main() {
	root := flag.String("root", ".", "配置与挂载根目录")
	check := flag.Bool("check", false, "离线校验配置、数据库、规则与会话格式后退出（可补全默认文件及迁移）")
	probe := flag.Bool("probe", false, "验证 Telegram 登录及规则源/目标可解析后退出，不转发消息")
	health := flag.Bool("healthcheck", false, "检查本容器运行状态")
	showVersion := flag.Bool("version", false, "显示版本")
	flag.Parse()
	if *showVersion {
		fmt.Printf("telegram-forwarder %s (%s)\n", version, commit)
		return
	}
	if *health {
		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8099/healthz")
		if err != nil {
			fmt.Fprintln(os.Stderr, "服务未就绪")
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *root, *check, *probe); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("程序退出", "error", err)
		os.Exit(1)
	}
}

type event struct {
	user     bool
	message  *model.Message
	callback *model.Callback
	done     chan error
}

func run(ctx context.Context, root string, check, probe bool) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	if err = cfg.Validate(); err != nil {
		return err
	}
	for _, path := range []string{"db", "sessions", "temp", "logs", "ufb/config"} {
		if err = os.MkdirAll(cfg.Path(path), 0700); err != nil {
			return err
		}
	}
	if !check && !probe {
		logFile, err := logging.Open(cfg.Path("logs/telegram_forwarder.log"))
		if err != nil {
			return err
		}
		defer logFile.Close()
		slog.SetDefault(slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, logFile), nil)))
	}
	store, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("数据库初始化: %w", err)
	}
	defer store.Close()
	store.ConfigureDefaults(cfg.Int("DEFAULT_MAX_MEDIA_SIZE", 10), cfg.String("DEFAULT_SUMMARY_TIME", "07:00"))
	ruleList, err := store.Rules(ctx)
	if err != nil {
		return err
	}
	if err = validateRules(ruleList); err != nil {
		return err
	}
	newClient := telegram.New
	if probe {
		newClient = telegram.NewProbe
	}
	user, err := newClient(cfg, false)
	if err != nil {
		return fmt.Errorf("用户会话: %w", err)
	}
	botClient, err := newClient(cfg, true)
	if err != nil {
		return fmt.Errorf("机器人会话: %w", err)
	}
	if check {
		slog.Info("离线校验通过", "rules", len(ruleList), "version", version)
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if probe {
		var deadline context.CancelFunc
		runCtx, deadline = context.WithTimeout(runCtx, 90*time.Second)
		defer deadline()
	}
	group, runCtx := errgroup.WithContext(runCtx)
	events := make(chan event, 256)
	queue := func(ctx context.Context, ev event) {
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	}
	queueMessage := func(ctx context.Context, msg model.Message, isUser bool) error {
		done := make(chan error, 1)
		queue(ctx, event{user: isUser, message: &msg, done: done})
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !probe {
		user.OnMessageAck = func(ctx context.Context, msg model.Message) error { return queueMessage(ctx, msg, true) }
		botClient.OnMessageAck = func(ctx context.Context, msg model.Message) error { return queueMessage(ctx, msg, false) }
		botClient.OnCallback = func(ctx context.Context, cb model.Callback) { queue(ctx, event{callback: &cb}) }
	}
	userReady, botReady := make(chan struct{}), make(chan struct{})
	group.Go(func() error {
		return user.Run(runCtx, func(ctx context.Context) error { close(userReady); <-ctx.Done(); return ctx.Err() })
	})
	group.Go(func() error {
		return botClient.Run(runCtx, func(ctx context.Context) error { close(botReady); <-ctx.Done(); return ctx.Err() })
	})
	for _, ready := range []<-chan struct{}{userReady, botReady} {
		select {
		case <-ready:
		case <-runCtx.Done():
			cancel()
			return group.Wait()
		}
	}
	if probe {
		slog.Info("Telegram 用户及机器人旧会话登录通过")
		var failures []error
		checkPeer := func(client *telegram.Client, id string) error {
			peer, err := client.Resolve(runCtx, id)
			if err != nil {
				return err
			}
			return client.CheckPeer(runCtx, peer)
		}
		for _, rule := range ruleList {
			if !rule.EnableRule {
				continue
			}
			if err := checkPeer(user, rule.Source.TelegramChatID); err != nil {
				failures = append(failures, fmt.Errorf("规则 %d 源会话: %w", rule.ID, err))
			}
			client := user
			if rule.UseBot {
				client = botClient
			}
			if err := checkPeer(client, rule.Target.TelegramChatID); err != nil {
				failures = append(failures, fmt.Errorf("规则 %d 目标会话: %w", rule.ID, err))
			}
		}
		cancel()
		_ = group.Wait()
		if len(failures) > 0 {
			return fmt.Errorf("Telegram 规则实体校验失败: %w", errors.Join(failures...))
		}
		slog.Info("Telegram 在线探测通过，未转发消息", "rules", len(ruleList))
		return nil
	}
	aiClient := ai.New(cfg)
	ufbClient := ufb.New(cfg, store)
	sched := scheduler.New(cfg, store, user, botClient, aiClient)
	controller := bot.New(cfg, store, user, botClient)
	controller.Summary, controller.Sync = sched.Summary, ufbClient.Sync
	engine := rules.New(cfg, store, user, botClient)
	engine.AI, engine.Push, engine.BotID = aiClient, push.New(cfg), botClient.SelfID()
	if cfg.Bool("RSS_ENABLED", false) {
		rssServer, err := rss.New(cfg, store, aiClient)
		if err != nil {
			cancel()
			_ = group.Wait()
			return err
		}
		engine.RSS = rssServer
		controller.CleanupRule = rssServer.RemoveRule
		addr := net.JoinHostPort(cfg.String("RSS_HOST", "0.0.0.0"), fmt.Sprint(cfg.Int("RSS_PORT", 8000)))
		group.Go(func() error { return serve(runCtx, addr, rssServer.Handler()) })
	}
	group.Go(func() error { engine.Run(runCtx); return runCtx.Err() })
	group.Go(func() error { return sched.Run(runCtx) })
	group.Go(func() error { return ufbClient.Run(runCtx) })
	group.Go(func() error {
		for {
			select {
			case <-runCtx.Done():
				return runCtx.Err()
			case ev := <-events:
				if ev.callback != nil {
					controller.HandleCallback(runCtx, *ev.callback)
					continue
				}
				msg := *ev.message
				if msg.Sender.ID == botClient.SelfID() {
					ev.done <- nil
					continue
				}
				if ev.user {
					if !controller.HandleInput(runCtx, msg) {
						engine.SubmitWithAck(runCtx, msg, ev.done)
						continue
					}
				} else {
					controller.HandleMessage(runCtx, msg)
				}
				ev.done <- nil
			}
		}
	})
	group.Go(func() error {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
			if runCtx.Err() != nil {
				http.Error(w, "stopping", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{\"status\":\"ready\"}\n")
		})
		return serve(runCtx, "127.0.0.1:8099", mux)
	})
	if err := botClient.RegisterCommands(runCtx, commands); err != nil {
		slog.Warn("注册机器人菜单失败", "error", err)
	}
	slog.Info("服务已就绪", "version", version, "rules", len(ruleList))
	return group.Wait()
}

func validateRules(ruleList []model.Rule) error {
	for _, rule := range ruleList {
		if !rule.EnableRule {
			continue
		}
		for _, keyword := range rule.Keywords {
			if keyword.IsRegex {
				if _, err := pyregex.Search(keyword.Keyword, ""); err != nil {
					slog.Warn("无效正则关键词将在处理时跳过", "rule", rule.ID, "keyword", keyword.ID)
				}
			}
		}
		for _, replacement := range rule.Replacements {
			if _, err := pyregex.Replace(replacement.Pattern, replacement.Content, ""); err != nil {
				slog.Warn("无效替换规则将在处理时跳过", "rule", rule.ID, "replacement", replacement.ID)
			}
		}
		if rule.EnableRule && rule.EnablePush {
			for _, channel := range rule.PushConfigs {
				if channel.EnablePushChannel {
					if err := push.Validate(channel.PushChannel); err != nil {
						return fmt.Errorf("规则 %d 推送配置 %d: %w", rule.ID, channel.ID, err)
					}
				}
			}
		}
	}
	return nil
}

func serve(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	err := server.ListenAndServe()
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return ctx.Err()
	}
	return fmt.Errorf("监听 %s: %w", addr, err)
}

// 菜单与旧版命令保持同名，全部别名仍由 bot 包解析。
var commands = func() []telegram.Command {
	text := `start 开始使用
help 查看帮助
bind 绑定源聊天
settings 管理转发规则
switch 切换当前规则
add 添加关键字
add_regex 添加正则关键字
add_all 添加关键字到所有规则
add_regex_all 添加正则关键字到所有规则
list_keyword 列出所有关键字
remove_keyword 删除关键字
remove_keyword_by_id 按序号删除关键字
remove_all_keyword 删除所有规则的指定关键字
replace 添加替换规则
replace_all 添加替换规则到所有规则
list_replace 列出替换规则
remove_replace 删除替换规则
export_keyword 导出关键字
export_replace 导出替换规则
import_keyword 导入普通关键字
import_regex_keyword 导入正则关键字
import_replace 导入替换规则
ufb_bind 绑定UFB域名
ufb_unbind 解绑UFB域名
ufb_item_change 切换UFB同步配置类型
clear_all_keywords 清除所有关键字
clear_all_keywords_regex 清除正则关键字
clear_all_replace 清除替换规则
copy_keywords 复制普通关键字
copy_keywords_regex 复制正则关键字
copy_replace 复制替换规则
copy_rule 复制规则
changelog 查看更新日志
list_rule 列出转发规则
delete_rule 删除转发规则
delete_rss_user 删除RSS用户
summary 立即总结`
	var result []telegram.Command
	for _, line := range strings.Split(text, "\n") {
		name, description, _ := strings.Cut(line, " ")
		result = append(result, telegram.Command{Name: name, Description: description})
	}
	return result
}()
