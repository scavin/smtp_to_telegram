package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	stdlog "log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	stdmail "net/mail"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	units "github.com/docker/go-units"
	"github.com/jhillyerd/enmime/v2"
	"github.com/urfave/cli/v2"
)

var (
	Version string = "UNKNOWN_RELEASE"
	logger         = NewAppLogger()
)

const (
	BodyTruncated = "\n\n[truncated]"
)

type SmtpConfig struct {
	smtpListen          string
	smtpPrimaryHost     string
	smtpMaxEnvelopeSize int64
	authToken           string
	authUsername        string
	authPassword        string
	logLevel            string
}

type TelegramConfig struct {
	telegramChatIds                  string
	telegramBotToken                 string
	telegramApiPrefix                string
	telegramApiTimeoutSeconds        float64
	messageTemplate                  string
	forwardedAttachmentMaxSize       int
	forwardedAttachmentMaxPhotoSize  int
	forwardedAttachmentRespectErrors bool
	messageLengthToSendAsFile        uint
}

type TelegramAPIMessageResult struct {
	Ok     bool                `json:"ok"`
	Result *TelegramAPIMessage `json:"result"`
}

type TelegramAPIMessage struct {
	// https://core.telegram.org/bots/api#message
	MessageId json.Number `json:"message_id"`
}

type FormattedEmail struct {
	text        string
	attachments []*FormattedAttachment
}

const (
	ATTACHMENT_TYPE_DOCUMENT = iota
	ATTACHMENT_TYPE_PHOTO    = iota
)

type FormattedAttachment struct {
	filename string
	caption  string
	content  []byte
	fileType int
}

type AppLogger struct {
	logger *stdlog.Logger
}

type SMTPDaemon struct {
	listener net.Listener
	wg       sync.WaitGroup
	closed   chan struct{}
	once     sync.Once
}

type SMTPEnvelope struct {
	MailFrom string
	RcptTo   []string
	Data     []byte
}

func NewAppLogger() *AppLogger {
	return &AppLogger{
		logger: stdlog.New(os.Stdout, "", stdlog.LstdFlags),
	}
}

func (l *AppLogger) Info(msg string) {
	l.logger.Printf("INFO %s", msg)
}

func (l *AppLogger) Error(msg string) {
	l.logger.Printf("ERROR %s", msg)
}

func (l *AppLogger) Errorf(format string, args ...interface{}) {
	l.logger.Printf("ERROR "+format, args...)
}

func (d *SMTPDaemon) Shutdown() {
	d.once.Do(func() {
		close(d.closed)
		if d.listener != nil {
			_ = d.listener.Close()
		}
		d.wg.Wait()
	})
}

func GetHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		panic(fmt.Sprintf("Unable to detect hostname: %s", err))
	}
	return hostname
}

func main() {
	app := cli.NewApp()
	app.Name = "smtp_to_telegram"
	app.Usage = "A simple program that listens for SMTP and forwards " +
		"all incoming Email messages to Telegram."
	app.Version = Version
	app.Action = func(c *cli.Context) error {
		smtpMaxEnvelopeSize, err := units.FromHumanSize(c.String("smtp-max-envelope-size"))
		if err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
		smtpConfig := &SmtpConfig{
			smtpListen:          c.String("smtp-listen"),
			smtpPrimaryHost:     c.String("smtp-primary-host"),
			smtpMaxEnvelopeSize: smtpMaxEnvelopeSize,
			authToken:           c.String("auth-token"),
			authUsername:        c.String("auth-username"),
			authPassword:        c.String("auth-password"),
			logLevel:            c.String("log-level"),
		}
		forwardedAttachmentMaxSize, err := units.FromHumanSize(c.String("forwarded-attachment-max-size"))
		if err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
		forwardedAttachmentMaxPhotoSize, err := units.FromHumanSize(c.String("forwarded-attachment-max-photo-size"))
		if err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
		telegramConfig := &TelegramConfig{
			telegramChatIds:                  c.String("telegram-chat-ids"),
			telegramBotToken:                 c.String("telegram-bot-token"),
			telegramApiPrefix:                c.String("telegram-api-prefix"),
			telegramApiTimeoutSeconds:        c.Float64("telegram-api-timeout-seconds"),
			messageTemplate:                  c.String("message-template"),
			forwardedAttachmentMaxSize:       int(forwardedAttachmentMaxSize),
			forwardedAttachmentMaxPhotoSize:  int(forwardedAttachmentMaxPhotoSize),
			forwardedAttachmentRespectErrors: c.Bool("forwarded-attachment-respect-errors"),
			messageLengthToSendAsFile:        c.Uint("message-length-to-send-as-file"),
		}
		d, err := SmtpStart(smtpConfig, telegramConfig)
		if err != nil {
			panic(fmt.Sprintf("start error: %s", err))
		}
		sigHandler(d)
		return nil
	}
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "smtp-listen",
			Value:   "127.0.0.1:2525",
			Usage:   "SMTP: TCP address to listen to",
			EnvVars: []string{"ST_SMTP_LISTEN"},
		},
		&cli.StringFlag{
			Name:    "smtp-primary-host",
			Value:   GetHostname(),
			Usage:   "SMTP: primary host",
			EnvVars: []string{"ST_SMTP_PRIMARY_HOST"},
		},
		&cli.StringFlag{
			Name:    "smtp-max-envelope-size",
			Usage:   "Max size of an incoming Email. Examples: 5k, 10m.",
			Value:   "50m",
			EnvVars: []string{"ST_SMTP_MAX_ENVELOPE_SIZE"},
		},
		&cli.StringFlag{
			Name:    "auth-token",
			Usage:   "Legacy shared token expected in X-SMTP-To-Telegram-Token",
			EnvVars: []string{"ST_AUTH_TOKEN"},
		},
		&cli.StringFlag{
			Name:    "auth-username",
			Usage:   "SMTP AUTH username",
			EnvVars: []string{"ST_AUTH_USERNAME"},
		},
		&cli.StringFlag{
			Name:    "auth-password",
			Usage:   "SMTP AUTH password",
			EnvVars: []string{"ST_AUTH_PASSWORD"},
		},
		&cli.StringFlag{
			Name:     "telegram-chat-ids",
			Usage:    "Telegram: comma-separated list of chat ids",
			EnvVars:  []string{"ST_TELEGRAM_CHAT_IDS"},
			Required: true,
		},
		&cli.StringFlag{
			Name:     "telegram-bot-token",
			Usage:    "Telegram: bot token",
			EnvVars:  []string{"ST_TELEGRAM_BOT_TOKEN"},
			Required: true,
		},
		&cli.StringFlag{
			Name:    "telegram-api-prefix",
			Usage:   "Telegram: API url prefix",
			Value:   "https://api.telegram.org/",
			EnvVars: []string{"ST_TELEGRAM_API_PREFIX"},
		},
		&cli.StringFlag{
			Name:    "message-template",
			Usage:   "Telegram message template",
			Value:   "From: {from}\\nTo: {to}\\nSubject: {subject}\\n\\n{body}\\n\\n{attachments_details}",
			EnvVars: []string{"ST_TELEGRAM_MESSAGE_TEMPLATE"},
		},
		&cli.Float64Flag{
			Name:    "telegram-api-timeout-seconds",
			Usage:   "HTTP timeout used for requests to the Telegram API",
			Value:   30,
			EnvVars: []string{"ST_TELEGRAM_API_TIMEOUT_SECONDS"},
		},
		&cli.StringFlag{
			Name: "forwarded-attachment-max-size",
			Usage: "Max size of an attachment to be forwarded to telegram. " +
				"0 -- disable forwarding. Examples: 5k, 10m. " +
				"Telegram API has a 50m limit on their side.",
			Value:   "10m",
			EnvVars: []string{"ST_FORWARDED_ATTACHMENT_MAX_SIZE"},
		},
		&cli.StringFlag{
			Name: "forwarded-attachment-max-photo-size",
			Usage: "Max size of a photo attachment to be forwarded to telegram. " +
				"0 -- disable forwarding. Examples: 5k, 10m. " +
				"Telegram API has a 10m limit on their side.",
			Value:   "10m",
			EnvVars: []string{"ST_FORWARDED_ATTACHMENT_MAX_PHOTO_SIZE"},
		},
		&cli.BoolFlag{
			Name: "forwarded-attachment-respect-errors",
			Usage: "Reject the whole email if some attachments " +
				"could not have been forwarded",
			Value:   false,
			EnvVars: []string{"ST_FORWARDED_ATTACHMENT_RESPECT_ERRORS"},
		},
		&cli.UintFlag{
			Name: "message-length-to-send-as-file",
			Usage: "If message length is greater than this number, it is " +
				"sent truncated followed by a text file containing " +
				"the full message. Telegram API has a limit of 4096 chars per message. " +
				"The maximum text file size is determined by `forwarded-attachment-max-size`.",
			Value:   4095,
			EnvVars: []string{"ST_MESSAGE_LENGTH_TO_SEND_AS_FILE"},
		},
		&cli.StringFlag{
			Name:    "log-level",
			Usage:   "Logging level (info, debug, error, panic).",
			Value:   "info",
			EnvVars: []string{"ST_LOG_LEVEL"},
		},
	}
	err := app.Run(os.Args)
	if err != nil {
		fmt.Printf("%s\n", err)
		os.Exit(1)
	}
}

func SmtpStart(
	smtpConfig *SmtpConfig, telegramConfig *TelegramConfig) (*SMTPDaemon, error) {
	if err := ValidateAuthConfig(smtpConfig); err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", smtpConfig.smtpListen)
	if err != nil {
		return nil, err
	}

	daemon := &SMTPDaemon{
		listener: listener,
		closed:   make(chan struct{}),
	}
	daemon.wg.Add(1)
	go func() {
		defer daemon.wg.Done()
		daemon.serve(smtpConfig, telegramConfig)
	}()

	return daemon, nil
}

func SendEmailToTelegram(
	e *SMTPEnvelope,
	smtpConfig *SmtpConfig,
	telegramConfig *TelegramConfig,
) error {
	if err := AuthorizeEmail(e, smtpConfig); err != nil {
		return err
	}

	message, err := FormatEmail(e, telegramConfig)
	if err != nil {
		return err
	}

	client := http.Client{
		Timeout: time.Duration(telegramConfig.telegramApiTimeoutSeconds*1000) * time.Millisecond,
	}

	for _, chatId := range strings.Split(telegramConfig.telegramChatIds, ",") {
		sentMessage, err := SendMessageToChat(message, chatId, telegramConfig, &client)
		if err != nil {
			// If unable to send at least one message -- reject the whole email.
			return errors.New(SanitizeBotToken(err.Error(), telegramConfig.telegramBotToken))
		}

		for _, attachment := range message.attachments {
			err = SendAttachmentToChat(attachment, chatId, telegramConfig, &client, sentMessage)
			if err != nil {
				err = errors.New(SanitizeBotToken(err.Error(), telegramConfig.telegramBotToken))
				if telegramConfig.forwardedAttachmentRespectErrors {
					return err
				} else {
					logger.Errorf("Ignoring attachment sending error: %s", err)
				}
			}
		}
	}
	return nil
}

func (d *SMTPDaemon) serve(smtpConfig *SmtpConfig, telegramConfig *TelegramConfig) {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.closed:
				return
			default:
				logger.Errorf("SMTP accept error: %v", err)
				continue
			}
		}

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer conn.Close()
			handleSMTPConnection(conn, smtpConfig, telegramConfig)
		}()
	}
}

func handleSMTPConnection(conn net.Conn, smtpConfig *SmtpConfig, telegramConfig *TelegramConfig) {
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	session := &smtpSession{
		conn:           conn,
		reader:         reader,
		writer:         writer,
		smtpConfig:     smtpConfig,
		telegramConfig: telegramConfig,
	}
	if err := session.run(); err != nil && !errors.Is(err, io.EOF) {
		logger.Errorf("SMTP session error: %v", err)
	}
}

type smtpSession struct {
	conn           net.Conn
	reader         *bufio.Reader
	writer         *bufio.Writer
	smtpConfig     *SmtpConfig
	telegramConfig *TelegramConfig
	helo           string
	authenticated  bool
	mailFrom       string
	rcptTo         []string
}

func (s *smtpSession) run() error {
	if err := s.writeResponse("220 %s ESMTP smtp_to_telegram", s.smtpConfig.smtpPrimaryHost); err != nil {
		return err
	}

	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}

		cmd, arg := splitSMTPCommand(line)
		switch cmd {
		case "EHLO":
			s.resetTransaction()
			s.helo = arg
			if err := s.writeEhloResponse(); err != nil {
				return err
			}
		case "HELO":
			s.resetTransaction()
			s.helo = arg
			if err := s.writeResponse("250 %s", s.smtpConfig.smtpPrimaryHost); err != nil {
				return err
			}
		case "AUTH":
			if err := s.handleAuth(arg); err != nil {
				return err
			}
		case "MAIL":
			if err := s.handleMail(arg); err != nil {
				return err
			}
		case "RCPT":
			if err := s.handleRcpt(arg); err != nil {
				return err
			}
		case "DATA":
			if err := s.handleData(); err != nil {
				return err
			}
		case "RSET":
			s.resetTransaction()
			if err := s.writeResponse("250 OK"); err != nil {
				return err
			}
		case "NOOP":
			if err := s.writeResponse("250 OK"); err != nil {
				return err
			}
		case "QUIT":
			return s.writeResponse("221 Bye")
		default:
			if err := s.writeResponse("502 Command not implemented"); err != nil {
				return err
			}
		}
	}
}

func (s *smtpSession) writeEhloResponse() error {
	lines := []string{
		fmt.Sprintf("250-%s", s.smtpConfig.smtpPrimaryHost),
		fmt.Sprintf("250-SIZE %d", s.smtpConfig.smtpMaxEnvelopeSize),
		"250-8BITMIME",
	}
	if SMTPAuthEnabled(s.smtpConfig) {
		lines = append(lines, "250-AUTH PLAIN LOGIN")
	}
	lines = append(lines, "250 PIPELINING")
	for _, line := range lines {
		if _, err := fmt.Fprintf(s.writer, "%s\r\n", line); err != nil {
			return err
		}
	}
	return s.writer.Flush()
}

func (s *smtpSession) writeResponse(format string, args ...interface{}) error {
	if _, err := fmt.Fprintf(s.writer, format+"\r\n", args...); err != nil {
		return err
	}
	return s.writer.Flush()
}

func (s *smtpSession) handleAuth(arg string) error {
	if !SMTPAuthEnabled(s.smtpConfig) {
		return s.writeResponse("502 Authentication not enabled")
	}

	parts := strings.Fields(arg)
	if len(parts) == 0 {
		return s.writeResponse("501 Syntax: AUTH mechanism")
	}

	switch strings.ToUpper(parts[0]) {
	case "PLAIN":
		return s.handleAuthPlain(parts[1:])
	case "LOGIN":
		return s.handleAuthLogin(parts[1:])
	default:
		return s.writeResponse("504 Unsupported authentication mechanism")
	}
}

func (s *smtpSession) handleAuthPlain(args []string) error {
	payload := ""
	if len(args) > 0 {
		payload = args[0]
	} else {
		if err := s.writeResponse("334 "); err != nil {
			return err
		}
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return err
		}
		payload = strings.TrimRight(line, "\r\n")
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return s.writeResponse("535 Authentication failed")
	}
	parts := strings.Split(string(decoded), "\x00")
	if len(parts) < 3 {
		return s.writeResponse("535 Authentication failed")
	}
	username := parts[len(parts)-2]
	password := parts[len(parts)-1]
	return s.finishAuth(username, password)
}

func (s *smtpSession) handleAuthLogin(args []string) error {
	username := ""
	if len(args) > 0 {
		decoded, err := base64.StdEncoding.DecodeString(args[0])
		if err != nil {
			return s.writeResponse("535 Authentication failed")
		}
		username = string(decoded)
	} else {
		if err := s.writeResponse("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:"))); err != nil {
			return err
		}
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return err
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimRight(line, "\r\n"))
		if err != nil {
			return s.writeResponse("535 Authentication failed")
		}
		username = string(decoded)
	}

	if err := s.writeResponse("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:"))); err != nil {
		return err
	}
	line, err := s.reader.ReadString('\n')
	if err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimRight(line, "\r\n"))
	if err != nil {
		return s.writeResponse("535 Authentication failed")
	}
	return s.finishAuth(username, string(decoded))
}

func (s *smtpSession) finishAuth(username string, password string) error {
	if username == s.smtpConfig.authUsername && password == s.smtpConfig.authPassword {
		s.authenticated = true
		return s.writeResponse("235 Authentication successful")
	}
	return s.writeResponse("535 Authentication failed")
}

func (s *smtpSession) handleMail(arg string) error {
	if !s.authenticatedOrOpenRelay() {
		return s.writeResponse("530 Authentication required")
	}
	if s.helo == "" {
		return s.writeResponse("503 Send HELO/EHLO first")
	}
	if !strings.HasPrefix(strings.ToUpper(arg), "FROM:") {
		return s.writeResponse("501 Syntax: MAIL FROM:<address>")
	}
	address := parseSMTPPath(arg[5:])
	if address == "" {
		return s.writeResponse("501 Invalid sender address")
	}
	s.mailFrom = address
	s.rcptTo = nil
	return s.writeResponse("250 OK")
}

func (s *smtpSession) handleRcpt(arg string) error {
	if s.mailFrom == "" {
		return s.writeResponse("503 Need MAIL before RCPT")
	}
	if !strings.HasPrefix(strings.ToUpper(arg), "TO:") {
		return s.writeResponse("501 Syntax: RCPT TO:<address>")
	}
	address := parseSMTPPath(arg[3:])
	if address == "" {
		return s.writeResponse("501 Invalid recipient address")
	}
	s.rcptTo = append(s.rcptTo, address)
	return s.writeResponse("250 OK")
}

func (s *smtpSession) handleData() error {
	if len(s.rcptTo) == 0 {
		return s.writeResponse("503 Need RCPT before DATA")
	}
	if err := s.writeResponse("354 End data with <CR><LF>.<CR><LF>"); err != nil {
		return err
	}

	var data bytes.Buffer
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == ".\r\n" || line == ".\n" {
			break
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		data.WriteString(line)
		if int64(data.Len()) > s.smtpConfig.smtpMaxEnvelopeSize {
			s.resetTransaction()
			return s.writeResponse("552 Message exceeds fixed maximum message size")
		}
	}

	envelope := &SMTPEnvelope{
		MailFrom: s.mailFrom,
		RcptTo:   append([]string(nil), s.rcptTo...),
		Data:     data.Bytes(),
	}
	s.resetTransaction()
	if err := SendEmailToTelegram(envelope, s.smtpConfig, s.telegramConfig); err != nil {
		return s.writeResponse("421 Error: %s", err)
	}
	return s.writeResponse("250 OK")
}

func (s *smtpSession) resetTransaction() {
	s.mailFrom = ""
	s.rcptTo = nil
}

func (s *smtpSession) authenticatedOrOpenRelay() bool {
	return !SMTPAuthEnabled(s.smtpConfig) || s.authenticated
}

func splitSMTPCommand(line string) (string, string) {
	parts := strings.SplitN(line, " ", 2)
	cmd := strings.ToUpper(parts[0])
	if len(parts) == 1 {
		return cmd, ""
	}
	return cmd, strings.TrimSpace(parts[1])
}

func parseSMTPPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, " "); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.Trim(raw, "<>")
	return strings.TrimSpace(raw)
}

func ValidateAuthConfig(smtpConfig *SmtpConfig) error {
	hasUsername := smtpConfig.authUsername != ""
	hasPassword := smtpConfig.authPassword != ""
	if hasUsername != hasPassword {
		return errors.New("both auth username and auth password must be set together")
	}
	return nil
}

func SMTPAuthEnabled(smtpConfig *SmtpConfig) bool {
	return smtpConfig.authUsername != ""
}

func AuthorizeEmail(e *SMTPEnvelope, smtpConfig *SmtpConfig) error {
	if smtpConfig.authToken == "" {
		return nil
	}

	msg, err := stdmail.ReadMessage(e.NewReader())
	if err != nil {
		return fmt.Errorf("unable to parse message headers for auth: %v", err)
	}

	if msg.Header.Get("X-SMTP-To-Telegram-Token") == smtpConfig.authToken {
		return nil
	}

	return errors.New(
		"authentication failed: provide X-SMTP-To-Telegram-Token",
	)
}

func SendMessageToChat(
	message *FormattedEmail,
	chatId string,
	telegramConfig *TelegramConfig,
	client *http.Client,
) (*TelegramAPIMessage, error) {
	// The native golang's http client supports
	// http, https and socks5 proxies via HTTP_PROXY/HTTPS_PROXY env vars
	// out of the box.
	//
	// See: https://golang.org/pkg/net/http/#ProxyFromEnvironment
	resp, err := client.PostForm(
		// https://core.telegram.org/bots/api#sendmessage
		fmt.Sprintf(
			"%sbot%s/sendMessage?disable_web_page_preview=true",
			telegramConfig.telegramApiPrefix,
			telegramConfig.telegramBotToken,
		),
		url.Values{"chat_id": {chatId}, "text": {message.text}},
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, errors.New(fmt.Sprintf(
			"Non-200 response from Telegram: (%d) %s",
			resp.StatusCode,
			EscapeMultiLine(body),
		))
	}

	j, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error reading json body of sendMessage: %v", err)
	}
	result := &TelegramAPIMessageResult{}
	err = json.Unmarshal(j, result)
	if err != nil {
		return nil, fmt.Errorf("Error parsing json body of sendMessage: %v", err)
	}
	if result.Ok != true {
		return nil, fmt.Errorf("ok != true: %s", j)
	}
	return result.Result, nil
}

func SendAttachmentToChat(
	attachment *FormattedAttachment,
	chatId string,
	telegramConfig *TelegramConfig,
	client *http.Client,
	sentMessage *TelegramAPIMessage,
) error {
	buf := new(bytes.Buffer)
	w := multipart.NewWriter(buf)
	var method string
	// https://core.telegram.org/bots/api#sending-files
	if attachment.fileType == ATTACHMENT_TYPE_DOCUMENT {
		// https://core.telegram.org/bots/api#senddocument
		method = "sendDocument"
		panicIfError(w.WriteField("chat_id", chatId))
		panicIfError(w.WriteField("reply_to_message_id", fmt.Sprintf("%s", sentMessage.MessageId)))
		panicIfError(w.WriteField("caption", attachment.caption))
		// TODO maybe reuse files sent to multiple chats via file_id?
		dw, err := w.CreateFormFile("document", attachment.filename)
		panicIfError(err)
		_, err = dw.Write(attachment.content)
		panicIfError(err)
	} else if attachment.fileType == ATTACHMENT_TYPE_PHOTO {
		// https://core.telegram.org/bots/api#sendphoto
		method = "sendPhoto"
		panicIfError(w.WriteField("chat_id", chatId))
		panicIfError(w.WriteField("reply_to_message_id", fmt.Sprintf("%s", sentMessage.MessageId)))
		panicIfError(w.WriteField("caption", attachment.caption))
		// TODO maybe reuse files sent to multiple chats via file_id?
		dw, err := w.CreateFormFile("photo", attachment.filename)
		panicIfError(err)
		_, err = dw.Write(attachment.content)
		panicIfError(err)
	} else {
		panic(fmt.Errorf("Unknown file type %d", attachment.fileType))
	}
	w.Close()

	resp, err := client.Post(
		fmt.Sprintf(
			"%sbot%s/%s?disable_notification=true",
			telegramConfig.telegramApiPrefix,
			telegramConfig.telegramBotToken,
			method,
		),
		w.FormDataContentType(),
		buf,
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return errors.New(fmt.Sprintf(
			"Non-200 response from Telegram: (%d) %s",
			resp.StatusCode,
			EscapeMultiLine(body),
		))
	}
	return nil
}

func FormatEmail(e *SMTPEnvelope, telegramConfig *TelegramConfig) (*FormattedEmail, error) {
	reader := e.NewReader()
	env, err := enmime.ReadEnvelope(reader)
	if err != nil {
		return nil, fmt.Errorf("error occurred during email parsing: %v", err)
	}
	text := env.Text

	attachmentsDetails := []string{}
	attachments := []*FormattedAttachment{}

	doParts := func(emoji string, parts []*enmime.Part) {
		for _, part := range parts {
			if bytes.Compare(part.Content, []byte(env.Text)) == 0 {
				continue
			}
			if text == "" && part.ContentType == "text/plain" && part.FileName == "" {
				text = string(part.Content)
				continue
			}
			action := "discarded"
			contentType := GuessContentType(part.ContentType, part.FileName)
			if FileIsImage(contentType) && len(part.Content) <= telegramConfig.forwardedAttachmentMaxPhotoSize {
				action = "sending..."
				attachments = append(attachments, &FormattedAttachment{
					filename: part.FileName,
					caption:  part.FileName,
					content:  part.Content,
					fileType: ATTACHMENT_TYPE_PHOTO,
				})
			} else {
				if len(part.Content) <= telegramConfig.forwardedAttachmentMaxSize {
					action = "sending..."
					attachments = append(attachments, &FormattedAttachment{
						filename: part.FileName,
						caption:  part.FileName,
						content:  part.Content,
						fileType: ATTACHMENT_TYPE_DOCUMENT,
					})
				}
			}
			line := fmt.Sprintf(
				"- %s %s (%s) %s, %s",
				emoji,
				part.FileName,
				contentType,
				units.HumanSize(float64(len(part.Content))),
				action,
			)
			attachmentsDetails = append(attachmentsDetails, line)
		}
	}
	doParts("🔗", env.Inlines)
	doParts("📎", env.Attachments)
	for _, part := range env.OtherParts {
		line := fmt.Sprintf(
			"- ❔ %s (%s) %s, discarded",
			part.FileName,
			GuessContentType(part.ContentType, part.FileName),
			units.HumanSize(float64(len(part.Content))),
		)
		attachmentsDetails = append(attachmentsDetails, line)
	}
	for _, e := range env.Errors {
		logger.Errorf("Envelope error: %s", e.Error())
	}

	if text == "" {
		text = string(e.Data)
	}

	formattedAttachmentsDetails := ""
	if len(attachmentsDetails) > 0 {
		formattedAttachmentsDetails = fmt.Sprintf(
			"Attachments:\n%s",
			strings.Join(attachmentsDetails, "\n"),
		)
	}

	fullMessageText, truncatedMessageText := FormatMessage(
		e.MailFrom,
		JoinEmailAddresses(e.RcptTo),
		env.GetHeader("subject"),
		text,
		formattedAttachmentsDetails,
		telegramConfig,
	)
	if truncatedMessageText == "" { // no need to truncate
		return &FormattedEmail{
			text:        fullMessageText,
			attachments: attachments,
		}, nil
	} else {
		if len(fullMessageText) > telegramConfig.forwardedAttachmentMaxSize {
			return nil, fmt.Errorf(
				"The message length (%d) is larger than `forwarded-attachment-max-size` (%d)",
				len(fullMessageText),
				telegramConfig.forwardedAttachmentMaxSize,
			)
		}
		at := &FormattedAttachment{
			filename: "full_message.txt",
			caption:  "Full message",
			content:  []byte(fullMessageText),
			fileType: ATTACHMENT_TYPE_DOCUMENT,
		}
		attachments := append([]*FormattedAttachment{at}, attachments...)
		return &FormattedEmail{
			text:        truncatedMessageText,
			attachments: attachments,
		}, nil
	}
}

func FormatMessage(
	from string, to string, subject string, text string,
	formattedAttachmentsDetails string,
	telegramConfig *TelegramConfig,
) (string, string) {
	fullMessageText := strings.TrimSpace(
		strings.NewReplacer(
			"\\n", "\n",
			"{from}", from,
			"{to}", to,
			"{subject}", subject,
			"{body}", strings.TrimSpace(text),
			"{attachments_details}", formattedAttachmentsDetails,
		).Replace(telegramConfig.messageTemplate),
	)
	fullMessageRunes := []rune(fullMessageText)
	if uint(len(fullMessageRunes)) <= telegramConfig.messageLengthToSendAsFile {
		// No need to truncate
		return fullMessageText, ""
	}

	emptyMessageText := strings.TrimSpace(
		strings.NewReplacer(
			"\\n", "\n",
			"{from}", from,
			"{to}", to,
			"{subject}", subject,
			"{body}", strings.TrimSpace(fmt.Sprintf(".%s", BodyTruncated)),
			"{attachments_details}", formattedAttachmentsDetails,
		).Replace(telegramConfig.messageTemplate),
	)
	emptyMessageRunes := []rune(emptyMessageText)
	if uint(len(emptyMessageRunes)) >= telegramConfig.messageLengthToSendAsFile {
		// Impossible to truncate properly
		return fullMessageText, string(fullMessageRunes[:telegramConfig.messageLengthToSendAsFile])
	}

	maxBodyLength := telegramConfig.messageLengthToSendAsFile - uint(len(emptyMessageRunes))
	truncatedMessageText := strings.TrimSpace(
		strings.NewReplacer(
			"\\n", "\n",
			"{from}", from,
			"{to}", to,
			"{subject}", subject,
			// TODO cut by paragraphs + respect formatting
			"{body}", strings.TrimSpace(fmt.Sprintf("%s%s",
				string([]rune(strings.TrimSpace(text))[:maxBodyLength]), BodyTruncated)),
			"{attachments_details}", formattedAttachmentsDetails,
		).Replace(telegramConfig.messageTemplate),
	)
	if uint(len([]rune(truncatedMessageText))) > telegramConfig.messageLengthToSendAsFile {
		panic(fmt.Errorf("Unexpected length of truncated message:\n%d\n%s",
			maxBodyLength, truncatedMessageText))
	}
	return fullMessageText, truncatedMessageText
}

func GuessContentType(contentType string, filename string) string {
	if contentType != "application/octet-stream" {
		return contentType
	}
	guessedType := mime.TypeByExtension(filepath.Ext(filename))
	if guessedType != "" {
		return guessedType
	}
	return contentType // Give up
}

func FileIsImage(contentType string) bool {
	switch contentType {
	case
		// "image/gif",  // sent as a static image
		// "image/x-ms-bmp",  // rendered as document
		"image/jpeg",
		"image/png":
		return true
	}
	return false
}

func JoinEmailAddresses(a []string) string {
	return strings.Join(a, ", ")
}

func EscapeMultiLine(b []byte) string {
	// Apparently errors returned by smtp must not contain newlines,
	// otherwise the data after the first newline is not getting
	// to the parsed message.
	s := string(b)
	s = strings.Replace(s, "\r", "\\r", -1)
	s = strings.Replace(s, "\n", "\\n", -1)
	return s
}

func SanitizeBotToken(s string, botToken string) string {
	return strings.Replace(s, botToken, "***", -1)
}

func panicIfError(err error) {
	if err != nil {
		panic(err)
	}
}

func (e *SMTPEnvelope) NewReader() io.Reader {
	return bytes.NewReader(e.Data)
}

func sigHandler(d *SMTPDaemon) {
	signalChannel := make(chan os.Signal, 1)

	signal.Notify(signalChannel,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGINT,
		syscall.SIGKILL,
		os.Kill,
	)
	for range signalChannel {
		logger.Info("Shutdown signal caught")
		go func() {
			select {
			// exit if graceful shutdown not finished in 60 sec.
			case <-time.After(time.Second * 60):
				logger.Error("graceful shutdown timed out")
				os.Exit(1)
			}
		}()
		d.Shutdown()
		logger.Info("Shutdown completed, exiting.")
		return
	}
}
