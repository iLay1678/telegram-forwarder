package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

type nativeMedia struct {
	owner      *Client
	peer       model.Peer
	messageID  int
	dc         int
	location   tg.InputFileLocationClass
	attributes []tg.DocumentAttributeClass
	spoiler    bool
	raw        tg.MessageMediaClass
}

func (c *Client) extractMedia(msg *tg.Message) (model.Media, bool) {
	n := nativeMedia{owner: c, peer: c.cached(rawPeer(msg.PeerID)), messageID: msg.ID, raw: msg.Media}
	m := model.Media{MessageID: msg.ID, Kind: "document"}
	switch v := msg.Media.(type) {
	case *tg.MessageMediaPhoto:
		p, ok := v.Photo.(*tg.Photo)
		if !ok {
			return m, false
		}
		m.Kind, m.MIME = "photo", "image/jpeg"
		m.Filename = fmt.Sprintf("photo_%d.jpg", p.ID)
		n.dc, n.spoiler = p.DCID, v.Spoiler
		thumb := ""
		for _, raw := range p.Sizes {
			switch s := raw.(type) {
			case *tg.PhotoSize:
				if int64(s.Size) > m.Size {
					m.Size = int64(s.Size)
					thumb = s.Type
				}
			case *tg.PhotoSizeProgressive:
				for _, size := range s.Sizes {
					if int64(size) > m.Size {
						m.Size = int64(size)
						thumb = s.Type
					}
				}
			}
		}
		if thumb == "" {
			return m, false
		}
		n.location = &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: thumb}
	case *tg.MessageMediaDocument:
		d, ok := v.Document.(*tg.Document)
		if !ok {
			return m, false
		}
		m.MIME, m.Size = d.MimeType, d.Size
		n.dc, n.attributes, n.spoiler = d.DCID, d.Attributes, v.Spoiler
		for _, raw := range d.Attributes {
			switch a := raw.(type) {
			case *tg.DocumentAttributeFilename:
				m.Filename = filepath.Base(strings.ReplaceAll(a.FileName, "\\", "/"))
			case *tg.DocumentAttributeVideo:
				m.Kind = "video"
				if a.RoundMessage {
					m.Kind = "video_note"
				}
			case *tg.DocumentAttributeAudio:
				m.Kind = "audio"
				if a.Voice {
					m.Kind = "voice"
				}
			case *tg.DocumentAttributeSticker:
				m.Kind = "sticker"
			}
		}
		for _, raw := range d.Attributes {
			if _, ok := raw.(*tg.DocumentAttributeAnimated); ok {
				m.Kind = "gif"
			}
		}
		if m.Filename == "" {
			extension := ""
			if exts, _ := mime.ExtensionsByType(m.MIME); len(exts) > 0 {
				extension = exts[0]
			}
			m.Filename = fmt.Sprintf("file_%d%s", d.ID, extension)
		}
		n.location = &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}
	case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		return m, false
	default:
		m.Kind = "other"
	}
	m.Native = n
	return m, true
}

func mediaDestination(m model.Media, dest string) (string, error) {
	if dest == "" {
		return "", errors.New("下载目标路径为空")
	}
	info, err := os.Stat(dest)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if (info != nil && info.IsDir()) || strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, "\\") {
		if err = os.MkdirAll(dest, 0700); err != nil {
			return "", err
		}
		name := filepath.Base(strings.ReplaceAll(m.Filename, "\\", "/"))
		if name == "." || name == "" || name == "/" {
			name = fmt.Sprintf("media_%d", m.MessageID)
		}
		f, err := os.CreateTemp(dest, "tg-*-"+strings.ReplaceAll(name, "*", "_"))
		if err != nil {
			return "", err
		}
		path := f.Name()
		if err = f.Close(); err != nil {
			return "", err
		}
		return path, nil
	}
	if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", err
	}
	return dest, nil
}

func (c *Client) Download(ctx context.Context, m model.Media, dest string) (string, error) {
	if n, ok := m.Native.(nativeMedia); ok && n.owner != nil && n.owner != c {
		return n.owner.Download(ctx, m, dest)
	}
	path, err := mediaDestination(m, dest)
	if err != nil {
		return "", err
	}
	if m.Path != "" {
		sourceAbs, _ := filepath.Abs(m.Path)
		destAbs, _ := filepath.Abs(path)
		if sourceAbs == destAbs {
			return path, nil
		}
		in, err := os.Open(m.Path)
		if err != nil {
			return "", err
		}
		defer in.Close()
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return "", err
		}
		return path, closeErr
	}
	n, ok := m.Native.(nativeMedia)
	if !ok || n.location == nil {
		return "", fmt.Errorf("媒体 %q 无可下载文件", m.Kind)
	}
	select {
	case c.mediaSlots <- struct{}{}:
		defer func() { <-c.mediaSlots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	for attempt := 0; attempt < 2; attempt++ {
		err = c.downloadNative(ctx, n, path)
		if err == nil {
			return path, nil
		}
		if attempt == 0 && (tgerr.Is(err, "FILE_REFERENCE_EXPIRED") || tgerr.Is(err, "FILE_REFERENCE_INVALID")) {
			fresh, e := c.Fetch(ctx, n.peer, []int{n.messageID})
			if e != nil {
				return "", e
			}
			if len(fresh) == 0 || len(fresh[0].Media) == 0 {
				return "", errors.New("源消息不存在，无法刷新媒体引用")
			}
			n, ok = fresh[0].Media[0].Native.(nativeMedia)
			if !ok {
				return "", errors.New("媒体引用刷新失败")
			}
			continue
		}
		break
	}
	return "", fmt.Errorf("下载 Telegram 媒体: %w", err)
}

func (c *Client) downloadNative(ctx context.Context, n nativeMedia, path string) (err error) {
	rpc := c.api
	if n.dc > 0 {
		pool, e := c.client.DC(ctx, n.dc, 1)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, pool.Close()) }()
		rpc = tg.NewClient(rpcMiddleware{timeout: 45 * time.Second}.Handle(pool))
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = downloader.NewDownloader().Download(rpc, n.location).Stream(ctx, f)
	return err
}

func (c *Client) uploadMedia(ctx context.Context, m model.Media) (tg.InputMediaClass, error) {
	if n, ok := m.Native.(nativeMedia); ok && n.location == nil {
		return copyNativeMedia(n.raw)
	}
	path := m.Path
	temporary := ""
	if path == "" {
		var err error
		temporary, err = os.MkdirTemp("", "telegram-forwarder-*")
		if err != nil {
			return nil, err
		}
		defer func() {
			if err := os.RemoveAll(temporary); err != nil {
				slog.Error("清理上传临时文件失败", "error", err)
			}
		}()
		path, err = c.Download(ctx, m, temporary+string(os.PathSeparator))
		if err != nil {
			return nil, err
		}
	}
	select {
	case c.mediaSlots <- struct{}{}:
		defer func() { <-c.mediaSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	file, err := uploader.NewUploader(c.api).WithThreads(1).FromPath(ctx, path)
	if err != nil {
		return nil, err
	}
	n, _ := m.Native.(nativeMedia)
	if m.Kind == "photo" {
		return &tg.InputMediaUploadedPhoto{File: file, Spoiler: n.spoiler}, nil
	}
	attrs := n.attributes
	if len(attrs) == 0 {
		name := m.Filename
		if name == "" {
			name = filepath.Base(path)
		}
		attrs = []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}}
		switch m.Kind {
		case "video", "video_note":
			attrs = append(attrs, &tg.DocumentAttributeVideo{SupportsStreaming: true, RoundMessage: m.Kind == "video_note"})
		case "audio", "voice":
			attrs = append(attrs, &tg.DocumentAttributeAudio{Voice: m.Kind == "voice"})
		case "gif":
			attrs = append(attrs, &tg.DocumentAttributeAnimated{})
		}
	}
	mimeType := m.MIME
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(path))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return &tg.InputMediaUploadedDocument{File: file, MimeType: mimeType, Attributes: attrs, Spoiler: n.spoiler, ForceFile: m.Kind == "document"}, nil
}

func copyNativeMedia(raw tg.MessageMediaClass) (tg.InputMediaClass, error) {
	switch v := raw.(type) {
	case *tg.MessageMediaContact:
		return &tg.InputMediaContact{PhoneNumber: v.PhoneNumber, FirstName: v.FirstName, LastName: v.LastName, Vcard: v.Vcard}, nil
	case *tg.MessageMediaGeo:
		if p, ok := v.Geo.(*tg.GeoPoint); ok {
			return &tg.InputMediaGeoPoint{GeoPoint: &tg.InputGeoPoint{Lat: p.Lat, Long: p.Long}}, nil
		}
	case *tg.MessageMediaDice:
		return &tg.InputMediaDice{Emoticon: v.Emoticon}, nil
	case *tg.MessageMediaVenue:
		if p, ok := v.Geo.(*tg.GeoPoint); ok {
			return &tg.InputMediaVenue{GeoPoint: &tg.InputGeoPoint{Lat: p.Lat, Long: p.Long}, Title: v.Title, Address: v.Address, Provider: v.Provider, VenueID: v.VenueID, VenueType: v.VenueType}, nil
		}
	case *tg.MessageMediaPoll:
		poll := v.Poll
		poll.ID = 0
		poll.Closed = false
		return &tg.InputMediaPoll{Poll: poll}, nil
	}
	return nil, fmt.Errorf("不支持重新发送的 Telegram 媒体 %T", raw)
}
