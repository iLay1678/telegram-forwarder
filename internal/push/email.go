package push

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/model"
)

type mailSettings struct {
	host, port, mode, username, password string
	from                                 *mail.Address
	to, cc, bcc                          []*mail.Address
}

// 常用邮箱遵循 Apprise 的 SMTP 自动配置；自建服务可使用 smtp、mode 和端口参数。
var mailDefaults = map[string][2]string{
	"gmail.com": {"smtp.gmail.com", "starttls"}, "qq.com": {"smtp.qq.com", "starttls"},
	"163.com": {"smtp.163.com", "ssl"}, "hotmail.com": {"smtp-mail.outlook.com", "starttls"},
	"outlook.com": {"smtp.outlook.com", "starttls"}, "live.com": {"smtp-mail.outlook.com", "starttls"},
	"office365.com": {"smtp.office365.com", "starttls"}, "yahoo.com": {"smtp.mail.yahoo.com", "starttls"},
	"fastmail.com": {"smtp.fastmail.com", "ssl"}, "zoho.com": {"smtp.zoho.com", "starttls"},
	"yandex.ru": {"smtp.yandex.ru", "ssl"}, "comcast.net": {"smtp.comcast.net", "ssl"},
}

func mailConfig(u *url.URL) (mailSettings, error) {
	q := u.Query()
	m := mailSettings{host: u.Hostname(), port: u.Port(), mode: q.Get("mode")}
	if u.User != nil {
		m.username = u.User.Username()
		m.password, _ = u.User.Password()
	}
	from := q.Get("from")
	if from == "" {
		from = m.username
		if from == "" {
			from = "apprise"
		}
		if !strings.Contains(from, "@") {
			from += "@" + u.Hostname()
		}
	}
	var err error
	if m.from, err = mail.ParseAddress(from); err != nil {
		return m, errors.New("邮件推送发件地址无效")
	}
	if host := q.Get("smtp"); host != "" {
		m.host = host
	} else if defaults, ok := mailDefaults[strings.ToLower(u.Hostname())]; ok && m.username != "" {
		m.host = defaults[0]
		if m.mode == "" {
			m.mode = defaults[1]
		}
		if !strings.Contains(m.username, "@") {
			m.username += "@" + u.Hostname()
		}
	}
	if m.mode == "" {
		m.mode = "insecure"
		if u.Scheme == "mailtos" {
			m.mode = "starttls"
		}
	}
	if m.mode != "insecure" && m.mode != "starttls" && m.mode != "ssl" {
		return m, errors.New("邮件 mode 必须是 insecure、starttls 或 ssl")
	}
	if m.port == "" {
		m.port = "25"
		if m.mode == "ssl" {
			m.port = "465"
		} else if m.mode == "starttls" {
			m.port = "587"
		}
	}
	parseList := func(raw string) ([]*mail.Address, error) {
		if raw == "" {
			return nil, nil
		}
		return mail.ParseAddressList(raw)
	}
	to := append(targetParts(u), q["to"]...)
	if m.to, err = parseList(strings.Join(to, ",")); err != nil {
		return m, errors.New("邮件推送收件地址无效")
	}
	if len(m.to) == 0 {
		m.to = []*mail.Address{m.from}
	}
	if m.cc, err = parseList(q.Get("cc")); err != nil {
		return m, errors.New("邮件推送抄送地址无效")
	}
	if m.bcc, err = parseList(q.Get("bcc")); err != nil {
		return m, errors.New("邮件推送密送地址无效")
	}
	return m, nil
}

func (c *Client) email(ctx context.Context, u *url.URL, text string, media []model.Media) error {
	m, err := mailConfig(u)
	if err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	var conn net.Conn
	if m.mode == "ssl" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", net.JoinHostPort(m.host, m.port))
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(m.host, m.port))
	}
	if err != nil {
		return errors.New("邮件 SMTP 连接失败")
	}
	defer conn.Close()
	deadline := time.Now().Add(60 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	client, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return errors.New("邮件 SMTP 握手失败")
	}
	defer client.Close()
	if m.mode == "starttls" {
		if err = client.StartTLS(&tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}); err != nil {
			return errors.New("邮件 SMTP STARTTLS 失败")
		}
	}
	if m.username != "" && m.password != "" {
		if err = client.Auth(smtp.PlainAuth("", m.username, m.password, m.host)); err != nil {
			return errors.New("邮件 SMTP 认证失败")
		}
	}
	if err = client.Mail(m.from.Address); err != nil {
		return errors.New("邮件 SMTP 拒绝发件人")
	}
	for _, group := range [][]*mail.Address{m.to, m.cc, m.bcc} {
		for _, address := range group {
			if err = client.Rcpt(address.Address); err != nil {
				return errors.New("邮件 SMTP 拒绝收件人")
			}
		}
	}
	writer, err := client.Data()
	if err != nil {
		return errors.New("邮件 SMTP 无法发送正文")
	}
	if err = writeMail(writer, m, u.Query(), text, media); err != nil {
		_ = writer.Close()
		return err
	}
	if err = writer.Close(); err != nil {
		return errors.New("邮件 SMTP 未确认接收")
	}
	return client.Quit()
}

func writeMail(w io.Writer, m mailSettings, q url.Values, text string, media []model.Media) error {
	addresses := func(group []*mail.Address) string {
		values := []string{}
		for _, a := range group {
			values = append(values, a.String())
		}
		return strings.Join(values, ", ")
	}
	// Header 值不能包含换行，避免通过标题或 URL 参数注入其他收件人。
	clean := func(s string) string { return strings.NewReplacer("\r", "", "\n", " ").Replace(s) }
	if _, err := fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\n", m.from.String(), addresses(m.to), mime.QEncoding.Encode("utf-8", clean(q.Get("title"))), time.Now().Format(time.RFC1123Z)); err != nil {
		return err
	}
	if len(m.cc) > 0 {
		if _, err := fmt.Fprintf(w, "Cc: %s\r\n", addresses(m.cc)); err != nil {
			return err
		}
	}
	if reply := q.Get("reply"); reply != "" {
		if a, err := mail.ParseAddress(reply); err == nil {
			if _, err = fmt.Fprintf(w, "Reply-To: %s\r\n", a.String()); err != nil {
				return err
			}
		}
	}
	parts := multipart.NewWriter(w)
	if _, err := fmt.Fprintf(w, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", parts.Boundary()); err != nil {
		return err
	}
	typ := "text/plain"
	if q.Get("format") == "html" {
		typ = "text/html"
	}
	part, err := parts.CreatePart(textproto.MIMEHeader{"Content-Type": {typ + "; charset=utf-8"}, "Content-Transfer-Encoding": {"base64"}})
	if err != nil {
		return err
	}
	if err = base64Mail(part, strings.NewReader(text)); err != nil {
		return err
	}
	for _, file := range media {
		name := file.Filename
		if name == "" {
			name = filepath.Base(file.Path)
		}
		typ = file.MIME
		if typ == "" {
			typ = mime.TypeByExtension(filepath.Ext(name))
		}
		if typ == "" {
			typ = "application/octet-stream"
		}
		part, err = parts.CreatePart(textproto.MIMEHeader{"Content-Type": {typ}, "Content-Disposition": {mime.FormatMediaType("attachment", map[string]string{"filename": name})}, "Content-Transfer-Encoding": {"base64"}})
		if err != nil {
			return err
		}
		reader, err := os.Open(file.Path)
		if err != nil {
			return err
		}
		err = base64Mail(part, reader)
		reader.Close()
		if err != nil {
			return err
		}
	}
	return parts.Close()
}

// 每 76 个字符折行，保持 MIME 附件可被旧邮件客户端解码。
func base64Mail(w io.Writer, r io.Reader) error {
	buffer := make([]byte, 57)
	for {
		n, err := io.ReadFull(r, buffer)
		if n > 0 {
			if _, writeErr := io.WriteString(w, base64.StdEncoding.EncodeToString(buffer[:n])+"\r\n"); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
