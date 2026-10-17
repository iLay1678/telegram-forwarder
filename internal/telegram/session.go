// Package telegram 实现 MTProto 传输与旧 Telethon 会话兼容。
package telegram

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/updates"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	_ "modernc.org/sqlite"
)

// atomicJSON 保证会话和游标不会因进程退出留下部分 JSON。
func atomicJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func atomicWrite(path string, data []byte) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".telegram-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

type fileSession struct {
	path string
	mu   sync.Mutex
}

func (s *fileSession) LoadSession(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	return b, err
}
func (s *fileSession) StoreSession(_ context.Context, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return atomicWrite(s.path, b)
}

type legacySession struct {
	Session  *session.Data
	Peers    []model.Peer
	State    *updates.State
	Channels map[int64]int
}

// readTelethon 以只读 SQLite 连接读取原文件，不修改 journal_mode 或任何表。
func readTelethon(ctx context.Context, path string) (*legacySession, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	if len(u.Path) > 1 && u.Path[1] == ':' {
		u.Path = "/" + u.Path
	}
	u.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var dc, port int
	var address string
	var key []byte
	err = db.QueryRowContext(ctx, "SELECT dc_id, server_address, port, auth_key FROM sessions LIMIT 1").Scan(&dc, &address, &port, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取 Telethon sessions: %w", err)
	}
	if len(key) == 0 {
		return nil, session.ErrNotFound
	}
	if len(key) != 256 || dc <= 0 || address == "" || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("Telethon 会话字段无效（DC=%d，授权长度=%d）", dc, len(key))
	}
	hash := sha1.Sum(key)
	out := &legacySession{Session: &session.Data{DC: dc, Addr: net.JoinHostPort(address, strconv.Itoa(port)), AuthKey: key, AuthKeyID: append([]byte(nil), hash[12:]...)}, Channels: map[int64]int{}}
	if exists, err := tableExists(ctx, db, "entities"); err != nil {
		return nil, err
	} else if exists {
		rows, err := db.QueryContext(ctx, "SELECT id, hash, COALESCE(username, ''), COALESCE(name, '') FROM entities")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, access int64
			var username, name string
			if err = rows.Scan(&id, &access, &username, &name); err != nil {
				rows.Close()
				return nil, err
			}
			// 旧实体缓存可能包含 ID 为 0 的占位记录，它不是可寻址的会话。
			if id == 0 {
				continue
			}
			p, err := ParsePeerID(strconv.FormatInt(id, 10))
			if err != nil {
				rows.Close()
				return nil, err
			}
			p.AccessHash, p.Name, p.Username = access, name, username
			out.Peers = append(out.Peers, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if exists, err := tableExists(ctx, db, "update_state"); err != nil {
		return nil, err
	} else if exists {
		rows, err := db.QueryContext(ctx, "SELECT id, pts, qts, date, seq FROM update_state")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var s updates.State
			var date float64
			if err = rows.Scan(&id, &s.Pts, &s.Qts, &date, &s.Seq); err != nil {
				rows.Close()
				return nil, err
			}
			// Telethon 可在 INTEGER 列保存带小数的 Unix 时间；MTProto 使用整秒。
			s.Date = int(date)
			if id == 0 {
				copyState := s
				out.State = &copyState
			} else {
				out.Channels[id] = s.Pts
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n)
	return n > 0, err
}
