package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/gotd/td/telegram/updates"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

type persistedState struct {
	Users    map[int64]updates.State `json:"users"`
	Channels map[string]int          `json:"channels"`
	Hashes   map[string]int64        `json:"hashes"`
	Peers    map[string]model.Peer   `json:"peers,omitempty"`
}
type stateStore struct {
	mu   sync.Mutex
	path string
	data persistedState
}

func openState(path string) (*stateStore, error) {
	s := &stateStore{path: path, data: persistedState{Users: map[int64]updates.State{}, Channels: map[string]int{}, Hashes: map[string]int64{}, Peers: map[string]model.Peer{}}}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("读取 Telegram 状态 %s: %w", path, err)
		}
	}
	if s.data.Users == nil {
		s.data.Users = map[int64]updates.State{}
	}
	if s.data.Channels == nil {
		s.data.Channels = map[string]int{}
	}
	if s.data.Hashes == nil {
		s.data.Hashes = map[string]int64{}
	}
	if s.data.Peers == nil {
		s.data.Peers = map[string]model.Peer{}
	}
	peers, err := os.ReadFile(s.peersPath())
	if err == nil {
		if err = json.Unmarshal(peers, &s.data.Peers); err != nil {
			return nil, fmt.Errorf("读取 Telegram 实体缓存: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if len(s.data.Peers) > 0 {
		// 兼容早期将实体与更新游标保存在同一文件的 Go 状态。
		if err = s.savePeers(); err != nil {
			return nil, err
		}
	}
	if s.data.Peers == nil {
		s.data.Peers = map[string]model.Peer{}
	}
	return s, nil
}
func stateKey(user, channel int64) string { return fmt.Sprintf("%d:%d", user, channel) }
func peerKey(p model.Peer) string         { return p.Kind + ":" + strconv.FormatInt(p.ID, 10) }
func (s *stateStore) peersPath() string {
	return strings.TrimSuffix(s.path, filepath.Ext(s.path)) + ".peers.json"
}
func (s *stateStore) savePeers() error { return atomicJSON(s.peersPath(), s.data.Peers) }
func (s *stateStore) save() error {
	state := s.data
	state.Peers = nil
	return atomicJSON(s.path, state)
}

// initialize 一次性合并旧游标和频道授权，避免为每个实体重写磁盘状态。
func (s *stateStore) initialize(id int64, legacy *legacySession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if legacy != nil {
		if _, ok := s.data.Users[id]; !ok && legacy.State != nil {
			s.data.Users[id] = *legacy.State
		}
		for channel, pts := range legacy.Channels {
			key := stateKey(id, channel)
			if _, ok := s.data.Channels[key]; !ok {
				s.data.Channels[key] = pts
			}
		}
	}
	for _, peer := range s.data.Peers {
		if peer.Kind == "channel" && peer.AccessHash != 0 {
			s.data.Hashes[stateKey(id, peer.ID)] = peer.AccessHash
		}
	}
	return s.save()
}
func (s *stateStore) GetState(_ context.Context, id int64) (updates.State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data.Users[id]
	return v, ok, nil
}
func (s *stateStore) SetState(_ context.Context, id int64, v updates.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Users[id] = v
	return s.save()
}
func (s *stateStore) change(id int64, fn func(*updates.State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data.Users[id]
	if !ok {
		return fmt.Errorf("用户 %d 的更新状态不存在", id)
	}
	fn(&v)
	s.data.Users[id] = v
	return s.save()
}
func (s *stateStore) SetPts(_ context.Context, id int64, v int) error {
	return s.change(id, func(s *updates.State) { s.Pts = v })
}
func (s *stateStore) SetQts(_ context.Context, id int64, v int) error {
	return s.change(id, func(s *updates.State) { s.Qts = v })
}
func (s *stateStore) SetDate(_ context.Context, id int64, v int) error {
	return s.change(id, func(s *updates.State) { s.Date = v })
}
func (s *stateStore) SetSeq(_ context.Context, id int64, v int) error {
	return s.change(id, func(s *updates.State) { s.Seq = v })
}
func (s *stateStore) SetDateSeq(_ context.Context, id int64, d, q int) error {
	return s.change(id, func(s *updates.State) { s.Date = d; s.Seq = q })
}
func (s *stateStore) GetChannelPts(_ context.Context, id, ch int64) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data.Channels[stateKey(id, ch)]
	return v, ok, nil
}
func (s *stateStore) SetChannelPts(_ context.Context, id, ch int64, v int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Channels[stateKey(id, ch)] = v
	return s.save()
}
func (s *stateStore) ForEachChannels(ctx context.Context, id int64, fn func(context.Context, int64, int) error) error {
	s.mu.Lock()
	values := map[int64]int{}
	for k, v := range s.data.Channels {
		var u, ch int64
		if _, err := fmt.Sscanf(k, "%d:%d", &u, &ch); err != nil {
			s.mu.Unlock()
			return err
		}
		if u == id {
			values[ch] = v
		}
	}
	s.mu.Unlock()
	for ch, v := range values {
		if err := fn(ctx, ch, v); err != nil {
			return err
		}
	}
	return nil
}
func (s *stateStore) GetChannelAccessHash(_ context.Context, id, ch int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data.Hashes[stateKey(id, ch)]
	return v, ok, nil
}
func (s *stateStore) SetChannelAccessHash(_ context.Context, id, ch, v int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := stateKey(id, ch)
	if s.data.Hashes[k] == v {
		return nil
	}
	s.data.Hashes[k] = v
	return s.save()
}
func (s *stateStore) peer(p model.Peer) (model.Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data.Peers[peerKey(p)]
	return v, ok
}
func (s *stateStore) peers() []model.Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Peer, 0, len(s.data.Peers))
	for _, v := range s.data.Peers {
		out = append(out, v)
	}
	return out
}
func (s *stateStore) putPeers(peers []model.Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, p := range peers {
		if p.ID <= 0 {
			continue
		}
		key := peerKey(p)
		old, ok := s.data.Peers[key]
		if p.AccessHash == 0 {
			p.AccessHash = old.AccessHash
		}
		if p.Name == "" {
			p.Name = old.Name
		}
		if !ok || old != p {
			s.data.Peers[key] = p
			changed = true
		}
	}
	if changed {
		return s.savePeers()
	}
	return nil
}
