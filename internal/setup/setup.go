// Package setup is "stellwerk init": it sets up a Matrix bot account and
// an encrypted room, then writes a working config and an env file with the
// generated secrets. It is the step new users find hardest by hand.
package setup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnnycube/stellwerk/internal/adapters/out/target/matrix"
	"github.com/johnnycube/stellwerk/internal/config"
)

type Options struct {
	Homeserver string
	UserID     string
	Password   string
	Invite     []string
	RoomName   string
	Dir        string // where stellwerk.yaml, stellwerk.env and data/ go
	Force      bool   // overwrite existing files
}

// Files are the names written in Dir.
const (
	ConfigFile = "stellwerk.yaml"
	EnvFile    = "stellwerk.env"
)

func Run(ctx context.Context, o Options, out io.Writer, log *slog.Logger) error {
	if o.Homeserver == "" || o.UserID == "" || o.Password == "" {
		return fmt.Errorf("init: homeserver, user and password are required")
	}
	if len(o.Invite) == 0 {
		return fmt.Errorf("init: give at least one -invite, the person who gets the alerts")
	}
	if o.RoomName == "" {
		o.RoomName = "stellwerk"
	}
	if o.Dir == "" {
		o.Dir = "."
	}
	cfgPath, envPath := filepath.Join(o.Dir, ConfigFile), filepath.Join(o.Dir, EnvFile)
	if !o.Force {
		for _, p := range []string{cfgPath, envPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("init: %s exists, use -force to overwrite", p)
			}
		}
	}
	data := filepath.Join(o.Dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	secrets := map[string]string{
		"MATRIX_PASSWORD":    o.Password,
		"MATRIX_PICKLE_KEY":  random(32),
		"TOKEN_ALERTMANAGER": random(24),
		"TOKEN_DEFAULT":      random(24),
	}
	fmt.Fprintf(out, "Logging in as %s and setting up encryption ...\n", o.UserID)
	res, err := matrix.Setup(ctx, matrix.SetupRequest{
		Settings: matrix.Settings{
			Homeserver: o.Homeserver, UserID: o.UserID, Password: o.Password,
			Database: filepath.Join(data, "matrix-alerts-init.db"), PickleKey: secrets["MATRIX_PICKLE_KEY"], DeviceName: "stellwerk init",
		},
		Invite: o.Invite, RoomName: o.RoomName,
	}, log)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	secrets["MATRIX_RECOVERY_KEY"] = res.RecoveryKey

	yml := strings.NewReplacer("{HS}", o.Homeserver, "{USER}", o.UserID, "{ROOM}", res.RoomID, "{DATA}", data,
		"{INVITERS}", quoteList(o.Invite), "{ME}", o.Invite[0]).Replace(template)
	if _, err := config.Parse([]byte(os.Expand(yml, func(k string) string { return or(secrets[k], "x") }))); err != nil {
		return fmt.Errorf("init: the generated config does not validate, please report this: %w", err)
	}
	if err := os.WriteFile(cfgPath, []byte(yml), 0o600); err != nil {
		return err
	}
	var env strings.Builder
	env.WriteString("# Secrets for stellwerk.yaml. Keep this file private. Load it with: set -a; . ./stellwerk.env; set +a\n")
	for _, k := range []string{"MATRIX_PASSWORD", "MATRIX_PICKLE_KEY", "MATRIX_RECOVERY_KEY", "TOKEN_ALERTMANAGER", "TOKEN_DEFAULT"} {
		fmt.Fprintf(&env, "%s=%s\n", k, shellQuote(secrets[k]))
	}
	if err := os.WriteFile(envPath, []byte(env.String()), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, `
Done.
  room        %s (invited: %s)
  config      %s
  secrets     %s (mode 0600, never commit it)
`, res.RoomID, strings.Join(o.Invite, ", "), cfgPath, envPath)
	if res.Warning != "" {
		fmt.Fprintf(out, "  warning     %s\n", res.Warning)
	}
	fmt.Fprintf(out, `
Next:
  1. Accept the invite in Element.
  2. set -a; . %s; set +a
  3. stellwerk -config %s
  4. Point Alertmanager at http://<host>:8080/in/alertmanager with the token TOKEN_ALERTMANAGER,
     or try it: stellwerk send -source test -token "$TOKEN_DEFAULT" "hello"
`, envPath, cfgPath)
	return nil
}

const template = `# Written by "stellwerk init". Secrets come from stellwerk.env.
# Everything is explained in stellwerk.example.yaml.
sources:
  alertmanager: { kind: alertmanager, token: "${TOKEN_ALERTMANAGER}" }
  "*": { kind: generic, token: "${TOKEN_DEFAULT}" }

targets:
  matrix:
    type: matrix
    settings:
      homeserver: {HS}
      database: {DATA}/matrix-{account}-{node}.db
      allowed_inviters: [{INVITERS}]
    accounts:
      alerts:
        user_id: "{USER}"
        password: ${MATRIX_PASSWORD}
        pickle_key: ${MATRIX_PICKLE_KEY}
        recovery_key: ${MATRIX_RECOVERY_KEY}
        commands: true

destinations:
  alerts: { target: matrix, account: alerts, address: "{ROOM}" }

routes:
  - name: critical
    match: { source: alertmanager, status: firing, labels: { severity: "critical|page" } }
    to: alerts
    loud: true
    mention: ["{ME}"]
  - name: everything-else
    to: alerts

heartbeats:
  - { name: watchdog, source: alertmanager, labels: { alertname: Watchdog }, timeout: 10m, to: alerts }

delivery:
  spool: {DATA}/spool-{node}.db

audit:
  file: {DATA}/audit.jsonl
`

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func quoteList(l []string) string {
	q := make([]string, len(l))
	for i, s := range l {
		q[i] = `"` + s + `"`
	}
	return strings.Join(q, ", ")
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
