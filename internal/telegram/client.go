package telegram

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Client struct {
	cfg        *config.Config
	bot        bool
	probe      bool
	client     *gotd.Client
	api        *tg.Client
	state      *stateStore
	legacy     *legacySession
	manager    *updates.Manager
	updateGate *startupUpdates
	self       atomic.Int64
	mediaSlots chan struct{}
	OnMessage  func(context.Context, model.Message)
	// OnMessageAck 在处理完成后确认更新，退出时未处理的消息不提前推进磁盘游标。
	OnMessageAck func(context.Context, model.Message) error
	OnCallback   func(context.Context, model.Callback)
	seenMu       sync.Mutex
	seen         map[string]time.Time
	recoverMu    sync.Mutex
	recovering   map[int64]bool
}

var _ model.Transport = (*Client)(nil)

func New(cfg *config.Config, bot bool) (*Client, error) {
	return newClient(cfg, bot, false)
}

// NewProbe 仅建立授权与实体查询连接，不接收更新或推进消息游标。
func NewProbe(cfg *config.Config, bot bool) (*Client, error) {
	return newClient(cfg, bot, true)
}

func newClient(cfg *config.Config, bot, probe bool) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("Telegram 配置为空")
	}
	name := "user"
	if bot {
		name = "bot"
	}
	state, err := openState(cfg.Path("sessions/" + name + ".state.json"))
	if err != nil {
		return nil, err
	}
	c := &Client{cfg: cfg, bot: bot, probe: probe, state: state, mediaSlots: make(chan struct{}, 2), seen: make(map[string]time.Time), recovering: make(map[int64]bool)}
	storage := &fileSession{path: cfg.Path("sessions/" + name + ".gotd.json")}
	if _, err = storage.LoadSession(context.Background()); errors.Is(err, session.ErrNotFound) {
		legacy, readErr := readTelethon(context.Background(), cfg.Path("sessions/"+name+".session"))
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) && !errors.Is(readErr, session.ErrNotFound) {
			return nil, readErr
		}
		if legacy != nil {
			if err = (&session.Loader{Storage: storage}).Save(context.Background(), legacy.Session); err != nil {
				return nil, err
			}
			if err = state.putPeers(legacy.Peers); err != nil {
				return nil, err
			}
			c.legacy = legacy
		}
	} else if err != nil {
		return nil, err
	}
	// New 之后、授权完成之前退出，下一次仍需恢复尚未归属 self ID 的旧游标。
	if c.legacy == nil && len(state.data.Users) == 0 {
		legacy, readErr := readTelethon(context.Background(), cfg.Path("sessions/"+name+".session"))
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) && !errors.Is(readErr, session.ErrNotFound) {
			return nil, readErr
		}
		c.legacy = legacy
	}
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(os.Stderr), zap.WarnLevel))
	c.manager = updates.New(updates.Config{Handler: gotd.UpdateHandlerFunc(c.handleUpdates), Storage: state, AccessHasher: state, Logger: logger, OnChannelTooLong: func(id int64) {
		// gotd 在差异超过限制时只能推进游标，必须主动读取近期历史补回消息。
		slog.Warn("Telegram 频道更新差异已过期，开始自动历史补收", "channel_id", id)
		go c.recoverChannel(id)
	}})
	// 先把网络层收到的频道消息交给业务，再交给 gotd 的状态机维护
	// pts。gotd 在处理频道差异 timeout 时可能暂存更新数分钟；业务不应
	// 被这个游标维护窗口阻塞，后续重复分发由 dispatchMessage 去重。
	c.updateGate = &startupUpdates{handler: c.manager, realtime: c.handleRealtimeUpdates}
	c.client = gotd.NewClient(cfg.APIID, cfg.APIHash, gotd.Options{SessionStorage: storage, NoUpdates: probe, UpdateHandler: c.updateGate, Logger: logger, DialTimeout: 15 * time.Second, Middlewares: []gotd.Middleware{rpcMiddleware{timeout: 45 * time.Second}}, Device: gotd.DeviceConfig{DeviceModel: "Telegram Forwarder", SystemVersion: "Go", AppVersion: "2.0"}})
	c.api = c.client.API()
	return c, nil
}

func (c *Client) SelfID() int64 { return c.self.Load() }

func (c *Client) Run(ctx context.Context, ready func(context.Context) error) error {
	return c.client.Run(ctx, func(ctx context.Context) error {
		status, err := c.client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !status.Authorized {
			if c.bot {
				if c.cfg.BotToken == "" {
					return errors.New("机器人会话未授权，首次登录需要 BOT_TOKEN")
				}
				_, err = c.client.Auth().Bot(ctx, c.cfg.BotToken)
			} else {
				if c.cfg.PhoneNumber == "" {
					return errors.New("用户会话未授权，首次登录需要 PHONE_NUMBER")
				}
				a := &interactiveAuth{phone: c.cfg.PhoneNumber, reader: bufio.NewReader(os.Stdin)}
				err = auth.NewFlow(a, auth.SendCodeOptions{}).Run(ctx, c.client.Auth())
			}
			if err != nil {
				return fmt.Errorf("Telegram 登录: %w", err)
			}
		}
		me, err := c.client.Self(ctx)
		if err != nil {
			return err
		}
		if me.Bot != c.bot {
			return fmt.Errorf("会话身份与客户端模式不符（bot=%t）", c.bot)
		}
		c.self.Store(me.ID)
		if err = c.state.putPeers([]model.Peer{userPeer(me)}); err != nil {
			return err
		}
		if c.probe {
			if ready != nil {
				return ready(ctx)
			}
			<-ctx.Done()
			return ctx.Err()
		}
		if err = c.state.initialize(me.ID, c.legacy); err != nil {
			return err
		}
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		managerDone := make(chan error, 1)
		readyDone := make(chan error, 1)
		started := make(chan struct{})
		go func() {
			defer cancel()
			managerDone <- c.manager.Run(runCtx, c.api, me.ID, updates.AuthOptions{IsBot: me.Bot, OnStart: func(context.Context) { close(started) }})
		}()
		select {
		case err = <-managerDone:
			return err
		case <-started:
		case <-runCtx.Done():
			cancel()
			if managerErr := <-managerDone; managerErr != nil && !errors.Is(managerErr, context.Canceled) {
				return managerErr
			}
			return runCtx.Err()
		}
		go func() {
			defer cancel()
			if ready != nil {
				readyDone <- ready(runCtx)
			} else {
				<-runCtx.Done()
				readyDone <- runCtx.Err()
			}
		}()
		// 先通知调用方就绪，避免积压更新填满上层队列后阻塞两个客户端的启动屏障。
		if err = c.updateGate.start(runCtx); err != nil {
			cancel()
			managerErr, readyErr := <-managerDone, <-readyDone
			if managerErr != nil && !errors.Is(managerErr, context.Canceled) {
				return managerErr
			}
			if readyErr != nil && !errors.Is(readyErr, context.Canceled) {
				return readyErr
			}
			return err
		}
		select {
		case err = <-readyDone:
			cancel()
			managerErr := <-managerDone
			if err == nil && managerErr != nil && !errors.Is(managerErr, context.Canceled) {
				return managerErr
			}
			return err
		case err = <-managerDone:
			cancel()
			<-readyDone
			return err
		}
	})
}

// startupUpdates 暂存授权期间的更新，确保短消息也经过已启动的游标管理器。
type startupUpdates struct {
	mu       sync.Mutex
	ready    bool
	pending  []tg.UpdatesClass
	handler  gotd.UpdateHandler
	realtime func(context.Context, tg.UpdatesClass) error
}

func (s *startupUpdates) Handle(ctx context.Context, update tg.UpdatesClass) error {
	s.mu.Lock()
	if !s.ready {
		s.pending = append(s.pending, update)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if s.realtime != nil {
		if err := s.realtime(ctx, update); err != nil {
			// 即时分发失败时仍必须交给状态机，否则会丢失 pts 更新。
			slog.Warn("Telegram 实时消息分发失败，继续交给更新状态机", "error", err)
		}
	}
	return s.handler.Handle(ctx, update)
}

func (s *startupUpdates) start(ctx context.Context) error {
	s.mu.Lock()
	s.ready = true
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, update := range pending {
		if s.realtime != nil {
			if err := s.realtime(ctx, update); err != nil {
				slog.Warn("Telegram 启动阶段实时消息分发失败，继续交给更新状态机", "error", err)
			}
		}
		if err := s.handler.Handle(ctx, update); err != nil {
			return err
		}
	}
	return nil
}

type rpcMiddleware struct{ timeout time.Duration }

func (m rpcMiddleware) Handle(next tg.Invoker) gotd.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		for {
			callCtx, cancel := context.WithTimeout(ctx, m.timeout)
			err := next.Invoke(callCtx, input, output)
			cancel()
			wait, ok := tgerr.AsFloodWait(err)
			if !ok {
				return err
			}
			timer := time.NewTimer(wait + time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

type interactiveAuth struct {
	phone  string
	reader *bufio.Reader
}

func (a *interactiveAuth) ask(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, prompt)
	type answer struct {
		line string
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		line, err := a.reader.ReadString('\n')
		done <- answer{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-done:
		if result.err != nil {
			return "", fmt.Errorf("读取登录输入（首次登录需交互终端）: %w", result.err)
		}
		return strings.TrimSpace(result.line), nil
	}
}
func (a *interactiveAuth) Phone(ctx context.Context) (string, error) {
	if a.phone != "" {
		return a.phone, nil
	}
	return a.ask(ctx, "请输入 Telegram 手机号（含国家码）: ")
}
func (a *interactiveAuth) Password(ctx context.Context) (string, error) {
	return a.ask(ctx, "请输入 Telegram 两步验证密码: ")
}
func (a *interactiveAuth) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	return a.ask(ctx, "请输入 Telegram 登录验证码: ")
}
func (a *interactiveAuth) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("请先在 Telegram 官方客户端注册账号")
}
func (a *interactiveAuth) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errors.New("请先在 Telegram 官方客户端完成注册")
}
