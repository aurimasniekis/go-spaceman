// Command devpulse runs a Pulse-shaped NATS server for the local development stack (scripts/dev-stack.sh) and
// publishes sample events into it with the real go-pulse SDK. It is a development tool: never shipped, never for
// production data.
//
//	devpulse serve -dir DIR [-port 38422]       writes DIR/url, DIR/spaceman.creds and DIR/publisher.creds, runs until stopped
//	devpulse publish -dir DIR -subject S -type T [-id ID] FILE
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: devpulse serve|publish …")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "publish":
		err = publish(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "devpulse:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", "", "state directory")
	port := fs.Int("port", 38422, "client port")
	_ = fs.Parse(args)
	if *dir == "" {
		return fmt.Errorf("-dir is required")
	}
	s, err := pulsetest.New(filepath.Join(*dir, "jetstream"), *port)
	if err != nil {
		return err
	}
	defer s.Stop()
	for name, client := range map[string]string{"spaceman.creds": "spaceman", "publisher.creds": "pulse-github"} {
		c, err := s.CredsFor(client)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*dir, name), c, 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(*dir, "url"), []byte(s.URL+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, s.URL)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	return nil
}

func publish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	dir := fs.String("dir", "", "state directory of devpulse serve")
	subject := fs.String("subject", "", "event subject")
	typ := fs.String("type", "", "event type")
	id := fs.String("id", "", "event ID (default: generated)")
	_ = fs.Parse(args)
	if *dir == "" || *subject == "" || fs.NArg() != 1 {
		return fmt.Errorf("usage: devpulse publish -dir DIR -subject S -type T [-id ID] FILE")
	}
	url, err := os.ReadFile(filepath.Join(*dir, "url"))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	c, err := pulse.Connect(strings.TrimSpace(string(url)), pulse.WithCredsFile(filepath.Join(*dir, "publisher.creds")), pulse.WithSource("pulse-github"))
	if err != nil {
		return err
	}
	defer c.Close()
	var opts []pulse.PublishOption
	if *id != "" {
		opts = append(opts, pulse.WithID(*id))
	}
	ack, err := c.Publish(context.Background(), *subject, *typ, json.RawMessage(data), opts...)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "published %s seq=%d duplicate=%v\n", ack.ID, ack.Sequence, ack.Duplicate)
	return nil
}
