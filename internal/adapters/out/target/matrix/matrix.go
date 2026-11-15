// Package matrix is the Matrix target. Every account is an end-to-end
// encrypted bot user with its own device and key store.
//
// High availability: every node must be its own device with its own
// database. {node}, {account} and {target} in database are replaced.
// Set recovery_key so each new device cross-signs itself.
package matrix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	_ "github.com/mattn/go-sqlite3" // database/sql driver for the crypto store
	"github.com/rs/zerolog"
	"go.yaml.in/yaml/v3"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

func init() {
	target.Register(target.Info{
		Type:        "matrix",
		Description: "End-to-end encrypted Matrix bot account.",
		Address:     "room ID, e.g. !abc:example.org",
	}, func(ctx context.Context, env target.Env, n *yaml.Node) (target.Target, error) {
		var s Settings
		if err := target.DecodeSettings(n, &s); err != nil {
			return nil, err
		}
		if s.Homeserver == "" || s.UserID == "" || s.PickleKey == "" {
			return nil, fmt.Errorf("matrix %s/%s: homeserver, user_id and pickle_key are required", env.Target, env.Account)
		}
		if s.Database == "" {
			s.Database = "stellwerk-{target}-{account}-{node}.db"
		}
		s.Database = env.Expand(s.Database)
		if s.DeviceName == "" {
			s.DeviceName = "stellwerk {node}"
		}
		s.DeviceName = env.Expand(s.DeviceName)
		var commands target.CommandFunc
		if s.Commands {
			commands = env.Commands
		}
		return New(ctx, s, env.Log.With("target", env.Target, "account", env.Account), commands)
	})
}

type Settings struct {
	Homeserver      string   `yaml:"homeserver"`
	UserID          string   `yaml:"user_id"`
	Password        string   `yaml:"password"`
	DeviceName      string   `yaml:"device_name"`
	Database        string   `yaml:"database"`   // SQLite file, {node} {account} {target} are replaced
	PickleKey       string   `yaml:"pickle_key"` // encrypts keys at rest
	RecoveryKey     string   `yaml:"recovery_key"`
	AllowedInviters []string `yaml:"allowed_inviters"`
	// Commands: this account answers "!stellwerk ..." in its rooms. Turn it
	// on for one account per room, or every bot in the room answers.
	Commands bool `yaml:"commands"`
	// CommandUsers may send commands, default: allowed_inviters.
	CommandUsers []string `yaml:"command_users"`
}

// Matrix is an E2EE-capable sender built on mautrix-go. Keys and room state
// are stored in SQLite, so the device survives restarts. Lose that file and
// the bot becomes a new device.
type Matrix struct {
	client   *mautrix.Client
	crypto   *cryptohelper.CryptoHelper
	synced   atomic.Bool
	log      *slog.Logger
	password string
	started  time.Time

	mu      sync.Mutex
	watches map[string]chan struct{} // room + marker -> closed when seen
}

var (
	_ ports.Receiver       = (*Matrix)(nil)
	_ target.HealthChecker = (*Matrix)(nil)
)

func New(ctx context.Context, cfg Settings, log *slog.Logger, commands target.CommandFunc) (*Matrix, error) {
	client, err := mautrix.NewClient(cfg.Homeserver, id.UserID(cfg.UserID), "")
	if err != nil {
		return nil, err
	}
	client.Log = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).Level(zerolog.WarnLevel).With().Timestamp().Logger()

	ch, err := initCrypto(ctx, client, cfg)
	if err != nil {
		return nil, err
	}
	client.Crypto = ch

	m := &Matrix{client: client, crypto: ch, log: log, password: cfg.Password, started: time.Now(), watches: map[string]chan struct{}{}}
	commandUsers := cfg.CommandUsers
	if len(commandUsers) == 0 {
		commandUsers = cfg.AllowedInviters
	}

	if cfg.RecoveryKey != "" {
		m.verifyWithRecoveryKey(ctx, cfg.RecoveryKey)
	}

	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnSync(func(_ context.Context, _ *mautrix.RespSync, _ string) bool {
		m.synced.Store(true)
		return true
	})
	// Decrypted messages arrive here (the crypto helper decrypts and passes
	// them on): for read-back checks and for chat commands.
	syncer.OnEventType(event.EventMessage, m.onMessage(commands, commandUsers))
	syncer.OnEventType(event.StateMember, m.onMember(cfg.AllowedInviters))
	return m, nil
}

// initCrypto sets up the crypto helper and logs in if there is no stored device yet.
func initCrypto(ctx context.Context, client *mautrix.Client, cfg Settings) (*cryptohelper.CryptoHelper, error) {
	ch, err := cryptohelper.NewCryptoHelper(client, []byte(cfg.PickleKey), cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("crypto helper: %w", err)
	}
	localpart, _, err := id.UserID(cfg.UserID).Parse()
	if err != nil {
		return nil, fmt.Errorf("bad user_id: %w", err)
	}
	// Only used on the very first start (or when the DB is gone), afterwards
	// the stored access token and device are reused.
	ch.LoginAs = &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: localpart},
		Password:                 cfg.Password,
		InitialDeviceDisplayName: cfg.DeviceName,
	}
	if err := ch.Init(ctx); err != nil {
		return nil, fmt.Errorf("crypto init / login: %w", err)
	}
	return ch, nil
}

// verifyWithRecoveryKey cross-signs this device with the existing identity,
// so Element shows the bot as verified instead of "unverified device".
func (m *Matrix) verifyWithRecoveryKey(ctx context.Context, recoveryKey string) {
	if err := m.crypto.Machine().VerifyWithRecoveryKey(ctx, recoveryKey); err != nil {
		m.log.Warn("could not verify device with recovery key", "err", err)
	} else {
		m.log.Info("device cross-signed with recovery key")
	}
}

func (m *Matrix) onMessage(commands target.CommandFunc, commandUsers []string) mautrix.EventHandler {
	return func(ctx context.Context, evt *event.Event) {
		msg := evt.Content.AsMessage()
		if msg == nil {
			return
		}
		m.seen(evt.RoomID.String(), msg.Body)
		if commands == nil || evt.Sender == m.client.UserID || time.UnixMilli(evt.Timestamp).Before(m.started) {
			return // not ours to answer, our own, or history from before the start
		}
		if !slices.Contains(commandUsers, evt.Sender.String()) {
			return
		}
		reply, ok := commands(ctx, evt.ID.String(), evt.Sender.String(), msg.Body)
		if !ok || reply == "" {
			return
		}
		if err := m.Send(ctx, evt.RoomID.String(), domain.Message{Markdown: reply}); err != nil {
			m.log.Warn("command reply failed", "room", evt.RoomID, "err", err)
		}
	}
}

// onMember joins rooms on invites from allowed users.
func (m *Matrix) onMember(allowedInviters []string) mautrix.EventHandler {
	client, log := m.client, m.log
	return func(ctx context.Context, evt *event.Event) {
		if evt.GetStateKey() != client.UserID.String() || evt.Content.AsMember().Membership != event.MembershipInvite {
			return
		}
		if m, _ := client.StateStore.GetMember(ctx, evt.RoomID, client.UserID); m != nil && m.Membership == event.MembershipJoin {
			return // invite replayed after we already joined
		}
		if !slices.Contains(allowedInviters, evt.Sender.String()) {
			log.Warn("ignoring invite from non-allowed user", "room", evt.RoomID, "inviter", evt.Sender)
			return
		}
		if _, err := client.JoinRoomByID(ctx, evt.RoomID); err != nil {
			log.Error("join failed", "room", evt.RoomID, "err", err)
		} else {
			log.Info("joined room", "room", evt.RoomID, "inviter", evt.Sender)
		}
	}
}

// Run keeps syncing: sync delivers room membership and device lists, which
// are needed to share encryption keys with everyone in the room.
func (m *Matrix) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := m.client.SyncWithContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.log.Error("sync stopped, restarting", "err", err)
			time.Sleep(5 * time.Second)
		}
	}
}

func (m *Matrix) Ready() bool { return m.synced.Load() }

func (m *Matrix) Close() error { return m.crypto.Close() }

func (m *Matrix) Send(ctx context.Context, roomID string, msg domain.Message) error {
	if !m.Ready() {
		return errors.New("initial sync not finished")
	}
	rid := id.RoomID(roomID)
	// Never fall back to plaintext: if we do not know the room as encrypted
	// (not joined yet, state missing), fail and let the queue retry.
	encrypted, err := m.client.StateStore.IsEncrypted(ctx, rid)
	if err != nil {
		return err
	}
	if !encrypted {
		return fmt.Errorf("room %s is not known as encrypted (not joined or encryption off), refusing to send", roomID)
	}

	md := msg.Markdown
	userIDs := make([]id.UserID, 0, len(msg.Mentions))
	if len(msg.Mentions) > 0 {
		pills := make([]string, 0, len(msg.Mentions))
		for _, u := range msg.Mentions {
			userIDs = append(userIDs, id.UserID(u))
			pills = append(pills, fmt.Sprintf("[%s](https://matrix.to/#/%s)", u, u))
		}
		md = strings.Join(pills, " ") + " " + md
	}
	content := format.RenderMarkdown(md, true, false)
	// m.notice is not pushed by default push rules, m.text is.
	content.MsgType = event.MsgNotice
	if msg.Loud {
		content.MsgType = event.MsgText
	}
	// Intentional mentions: this is what makes clients push for a mention.
	content.Mentions = &event.Mentions{UserIDs: userIDs}

	_, err = m.client.SendMessageEvent(ctx, rid, event.EventMessage, &content)
	return classify(err)
}

// classify tells the core which homeserver errors are worth retrying.
func classify(err error) error {
	var he mautrix.HTTPError
	if err == nil || !errors.As(err, &he) || he.RespError == nil {
		return err
	}
	switch he.RespError.ErrCode {
	case "M_LIMIT_EXCEEDED":
		ms, _ := he.RespError.ExtraData["retry_after_ms"].(float64)
		return ports.RetryAfter(err, time.Duration(ms)*time.Millisecond)
	case "M_FORBIDDEN", "M_NOT_FOUND", "M_UNKNOWN_TOKEN", "M_MISSING_TOKEN", "M_USER_DEACTIVATED", "M_TOO_LARGE", "M_BAD_JSON", "M_NOT_JSON":
		return ports.Permanent(err) // not in the room, room gone, logged out: retrying does not help
	}
	return err
}

// Expect implements ports.Receiver: wait for a message containing marker in
// room. It sees what this account can decrypt, so it proves the room works
// for a member other than the sender.
func (m *Matrix) Expect(room, marker string) func(context.Context) error {
	key := room + "\x00" + marker
	ch := make(chan struct{})
	m.mu.Lock()
	m.watches[key] = ch
	m.mu.Unlock()
	return func(ctx context.Context) error {
		defer func() {
			m.mu.Lock()
			delete(m.watches, key)
			m.mu.Unlock()
		}()
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("%s did not receive the message in %s", m.client.UserID, room)
		}
	}
}

func (m *Matrix) seen(room, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, ch := range m.watches {
		r, marker, _ := strings.Cut(key, "\x00")
		if r == room && strings.Contains(body, marker) {
			close(ch)
			delete(m.watches, key)
		}
	}
}

// Check implements target.HealthChecker: everything that keeps messages
// from arriving readable, in words a person can act on.
func (m *Matrix) Check(ctx context.Context, rooms []string) []string {
	var problems []string
	if !m.Ready() {
		return []string{"initial sync not finished"}
	}
	if hasKeys, verified, err := m.crypto.Machine().GetOwnVerificationStatus(ctx); err == nil && (!hasKeys || !verified) {
		problems = append(problems, "this device is not cross-signed: run -bootstrap-cross-signing once and set recovery_key, or clients show the bot as unverified")
	}
	for _, r := range rooms {
		if p := m.roomProblem(ctx, r); p != "" {
			problems = append(problems, p)
		}
	}
	return problems
}

// roomProblem is what keeps room r from working, or "".
func (m *Matrix) roomProblem(ctx context.Context, r string) string {
	rid := id.RoomID(r)
	mem, err := m.client.StateStore.GetMember(ctx, rid, m.client.UserID)
	switch {
	case err != nil:
		return fmt.Sprintf("%s: %v", r, err)
	case mem == nil || mem.Membership != event.MembershipJoin:
		return fmt.Sprintf("%s: the bot is %s (invite it from an allowed_inviters account)", r, membershipState(mem))
	}
	if enc, err := m.client.StateStore.IsEncrypted(ctx, rid); err == nil && !enc {
		return fmt.Sprintf("%s: the room is not encrypted, stellwerk refuses to send there", r)
	}
	return ""
}

func membershipState(mem *event.MemberEventContent) string {
	if mem != nil {
		switch mem.Membership {
		case event.MembershipBan:
			return "banned"
		case event.MembershipInvite:
			return "invited but has not joined"
		default: // left, knocked: "not a member"
		}
	}
	return "not a member"
}

// BootstrapCrossSigning creates a cross-signing identity for the bot and
// returns the recovery key. Run once per bot account, not per node.
func (m *Matrix) BootstrapCrossSigning(ctx context.Context) (string, error) {
	mach := m.crypto.Machine()
	key, _, err := mach.GenerateAndUploadCrossSigningKeysWithPassword(ctx, m.password, "")
	if err != nil {
		return "", err
	}
	if err := mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
		return "", err
	}
	if err := mach.SignOwnMasterKey(ctx); err != nil {
		return "", err
	}
	return key, nil
}

// SetupRequest is what "stellwerk init" needs to set up a bot account.
type SetupRequest struct {
	Settings Settings // homeserver, user, password, database, pickle key
	Invite   []string // people to invite to the new room
	RoomName string
}

// SetupResult is what the new config needs.
type SetupResult struct {
	RoomID      string
	RecoveryKey string // empty if cross-signing could not be set up
	Warning     string
}

// Setup logs in (creating the device and its key store), sets up
// cross-signing, creates an encrypted room, invites people and posts a
// first message there.
func Setup(ctx context.Context, req SetupRequest, log *slog.Logger) (SetupResult, error) {
	var res SetupResult
	m, err := New(ctx, req.Settings, log, nil)
	if err != nil {
		return res, err
	}
	defer func() {
		if err := m.Close(); err != nil {
			log.Warn("matrix close failed", "err", err)
		}
	}()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go m.Run(runCtx)

	if key, err := m.BootstrapCrossSigning(ctx); err != nil {
		res.Warning = "cross-signing was not set up, clients will show the bot as unverified: " + err.Error()
	} else {
		res.RecoveryKey = key
	}
	invite := make([]id.UserID, 0, len(req.Invite))
	for _, u := range req.Invite {
		invite = append(invite, id.UserID(u))
	}
	// The people who asked for the room own it too. The creator is not
	// listed: newer room versions refuse that, the creator is admin anyway.
	power := &event.PowerLevelsEventContent{Users: map[id.UserID]int{}}
	for _, u := range invite {
		power.Users[u] = 100
	}
	room, err := m.client.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name: req.RoomName, Preset: "private_chat", Invite: invite, PowerLevelOverride: power,
		InitialState: []*event.Event{{Type: event.StateEncryption, Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}}}},
	})
	if err != nil {
		return res, fmt.Errorf("create room: %w", err)
	}
	res.RoomID = room.RoomID.String()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if enc, _ := m.client.StateStore.IsEncrypted(ctx, room.RoomID); enc && m.Ready() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := m.Send(ctx, res.RoomID, domain.Message{Markdown: "🚦 **stellwerk is set up.** Alerts routed to this room arrive here, end-to-end encrypted.\nTry `!stellwerk help` once commands are on."}); err != nil {
		res.Warning = strings.TrimSpace(res.Warning + "\nthe welcome message could not be sent: " + err.Error())
	}
	return res, nil
}
