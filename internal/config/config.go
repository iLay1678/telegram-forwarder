// Package config 读取旧版 dotenv 配置，保留环境变量优先级与显式空值。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type Config struct {
	Root         string
	APIID        int
	APIHash      string
	PhoneNumber  string
	BotToken     string
	UserID       int64
	Admins       []int64
	DatabasePath string
	Timezone     *time.Location
	TimezoneName string
	values       map[string]string
}

func Load(root string) (*Config, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	values, err := readDotEnv(filepath.Join(root, ".env"), os.Environ())
	if err != nil {
		return nil, fmt.Errorf("读取 .env: %w", err)
	}
	c := &Config{Root: root, values: values}
	c.APIHash = c.String("API_HASH", "")
	c.PhoneNumber = c.String("PHONE_NUMBER", "")
	c.BotToken = c.String("BOT_TOKEN", "")
	if value := c.String("API_ID", ""); value != "" {
		c.APIID, err = strconv.Atoi(value)
		if err != nil {
			return nil, errors.New("API_ID 必须是整数")
		}
	}
	if value := c.String("USER_ID", ""); value != "" {
		c.UserID, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, errors.New("USER_ID 必须是整数")
		}
	}
	if value := strings.TrimSpace(c.String("ADMINS", "")); value != "" {
		for _, item := range strings.Split(value, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(item), 10, 64)
			if err != nil || id <= 0 {
				return nil, errors.New("ADMINS 必须是逗号分隔的正整数")
			}
			c.Admins = append(c.Admins, id)
		}
	} else if c.UserID != 0 {
		c.Admins = []int64{c.UserID}
	}
	c.TimezoneName = c.String("DEFAULT_TIMEZONE", "Asia/Shanghai")
	c.Timezone, err = time.LoadLocation(c.TimezoneName)
	if err != nil {
		return nil, fmt.Errorf("DEFAULT_TIMEZONE 无效: %w", err)
	}
	c.DatabasePath, err = c.databasePath()
	if err != nil {
		return nil, err
	}
	if err := c.createDefaultFiles(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) String(key, fallback string) string {
	if value, ok := c.values[key]; ok {
		return value
	}
	return fallback
}

func (c *Config) Int(key string, fallback int) int {
	value, err := strconv.Atoi(c.String(key, strconv.Itoa(fallback)))
	if err != nil {
		return fallback
	}
	return value
}

func (c *Config) Bool(key string, fallback bool) bool {
	return strings.EqualFold(c.String(key, strconv.FormatBool(fallback)), "true")
}

func (c *Config) Path(rel string) string {
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}
	return filepath.Join(c.Root, filepath.FromSlash(rel))
}

func (c *Config) Validate() error {
	var missing []string
	if c.APIID <= 0 {
		missing = append(missing, "API_ID")
	}
	if c.APIHash == "" {
		missing = append(missing, "API_HASH")
	}
	if c.UserID <= 0 {
		missing = append(missing, "USER_ID")
	}
	if len(missing) != 0 {
		return fmt.Errorf("缺少有效必填配置: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) databasePath() (string, error) {
	legacy := c.Path("db/forward.db")
	value := c.String("DATABASE_URL", "sqlite:///./db/forward.db")
	if value == "" {
		return legacy, nil
	}
	if !strings.HasPrefix(value, "sqlite:///") {
		return "", errors.New("DATABASE_URL 仅支持 sqlite:/// 文件路径")
	}
	path := c.Path(strings.TrimPrefix(value, "sqlite:///"))
	samePath := path == legacy
	if requested, err := os.Stat(path); err == nil {
		if old, err := os.Stat(legacy); err == nil {
			samePath = os.SameFile(requested, old)
		}
	}
	if !samePath {
		if _, err := os.Stat(legacy); err == nil {
			return "", fmt.Errorf("DATABASE_URL 指向 %q，但发现旧版实际使用的数据库 %q；请将 DATABASE_URL 指向旧库或明确迁移文件后再启动", path, legacy)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("检查旧版数据库: %w", err)
		}
	}
	return path, nil
}

func (c *Config) createDefaultFiles() error {
	dir := c.Path("config")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for name, content := range defaultFiles {
		file, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("创建默认配置 %s: %w", name, err)
		}
		_, writeErr := file.WriteString(content)
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return fmt.Errorf("写入默认配置 %s: %w", name, err)
		}
	}
	return nil
}
