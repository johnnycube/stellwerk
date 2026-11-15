package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/johnnycube/stellwerk/internal/setup"
)

// runInit: stellwerk init -homeserver https://matrix.example.org -user @stellwerk:example.org -invite @me:example.org
func runInit(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stdout)
	var o setup.Options
	fs.StringVar(&o.Homeserver, "homeserver", "", "homeserver URL, e.g. https://matrix.example.org")
	fs.StringVar(&o.UserID, "user", "", "the bot's existing account, e.g. @stellwerk:example.org")
	invite := fs.String("invite", "", "who gets the alerts, comma separated, e.g. @me:example.org")
	fs.StringVar(&o.RoomName, "room-name", "stellwerk", "name of the new encrypted room")
	fs.StringVar(&o.Dir, "dir", ".", "where to write stellwerk.yaml, stellwerk.env and data/")
	fs.BoolVar(&o.Force, "force", false, "overwrite existing files")
	fs.Usage = func() {
		fmt.Fprintln(stdout, "usage: stellwerk init -homeserver URL -user @bot:server -invite @you:server")
		fmt.Fprintln(stdout, "The bot's password is read from $STELLWERK_INIT_PASSWORD or asked for.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, u := range strings.Split(*invite, ",") {
		if u = strings.TrimSpace(u); u != "" {
			o.Invite = append(o.Invite, u)
		}
	}
	o.Password = os.Getenv("STELLWERK_INIT_PASSWORD")
	if o.Password == "" {
		fmt.Fprintf(stdout, "Password for %s: ", o.UserID)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		o.Password = strings.TrimRight(line, "\r\n")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return setup.Run(ctx, o, stdout, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
}
