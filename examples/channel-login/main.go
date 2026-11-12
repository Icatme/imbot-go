// channel-login demonstrates the explicit credential enrollment step. The host
// renders Challenge.URL as a QR code; this example prints that authorization URL
// and writes credentials to a new private file without connecting or sending.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/Icatme/imbot-go/channels/dingtalk"
	"github.com/Icatme/imbot-go/channels/feishu"
	"github.com/Icatme/imbot-go/channels/onboarding"
	"github.com/Icatme/imbot-go/channels/qqbot"
	"github.com/Icatme/imbot-go/channels/wecom"
)

type credentials struct {
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	AppID        string `json:"app_id,omitempty"`
	AppSecret    string `json:"app_secret,omitempty"`
	Domain       string `json:"domain,omitempty"`
	OpenID       string `json:"open_id,omitempty"`
	BotID        string `json:"bot_id,omitempty"`
	Secret       string `json:"secret,omitempty"`
	UserOpenID   string `json:"user_openid,omitempty"`
}

type savedAccount struct {
	Platform    string      `json:"platform"`
	AccountID   string      `json:"account_id"`
	Credentials credentials `json:"credentials"`
}

type beginFunc func(context.Context, string) (onboarding.Challenge, func(context.Context) (credentials, error), error)

func begin(ctx context.Context, platform string) (onboarding.Challenge, func(context.Context) (credentials, error), error) {
	switch platform {
	case "dingtalk":
		registration, err := dingtalk.BeginRegistration(ctx, dingtalk.RegistrationOptions{})
		if err != nil {
			return onboarding.Challenge{}, nil, err
		}
		return registration.Challenge(), func(ctx context.Context) (credentials, error) {
			c, err := registration.Wait(ctx)
			return credentials{ClientID: c.ClientID, ClientSecret: c.ClientSecret}, err
		}, nil
	case "wecom":
		registration, err := wecom.BeginRegistration(ctx, wecom.RegistrationOptions{})
		if err != nil {
			return onboarding.Challenge{}, nil, err
		}
		return registration.Challenge(), func(ctx context.Context) (credentials, error) {
			c, err := registration.Wait(ctx)
			return credentials{BotID: c.BotID, Secret: c.Secret}, err
		}, nil
	case "qqbot":
		registration, err := qqbot.BeginRegistration(ctx, qqbot.RegistrationOptions{})
		if err != nil {
			return onboarding.Challenge{}, nil, err
		}
		return registration.Challenge(), func(ctx context.Context) (credentials, error) {
			c, err := registration.Wait(ctx)
			return credentials{AppID: c.AppID, ClientSecret: c.ClientSecret, UserOpenID: c.UserOpenID}, err
		}, nil
	case "feishu", "lark":
		registration, err := feishu.BeginRegistration(ctx, feishu.RegistrationOptions{Domain: platform})
		if err != nil {
			return onboarding.Challenge{}, nil, err
		}
		return registration.Challenge(), func(ctx context.Context) (credentials, error) {
			c, err := registration.Wait(ctx)
			return credentials{AppID: c.AppID, AppSecret: c.AppSecret, Domain: c.Domain, OpenID: c.OpenID}, err
		}, nil
	default:
		return onboarding.Challenge{}, nil, errors.New("unsupported registration platform")
	}
}

func run(ctx context.Context, args []string, out io.Writer, start beginFunc) error {
	flags := flag.NewFlagSet("channel-login", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	platform := flags.String("platform", "", "dingtalk, feishu, lark, wecom, or qqbot (required)")
	account := flags.String("account", "", "local account identifier (required)")
	output := flags.String("output", "", "new credential file in an existing directory (required)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(out)
			flags.PrintDefaults()
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || (*platform != "dingtalk" && *platform != "feishu" && *platform != "lark" && *platform != "wecom" && *platform != "qqbot") || strings.TrimSpace(*account) == "" || strings.TrimSpace(*output) == "" {
		return errors.New("provide -platform dingtalk|feishu|lark|wecom|qqbot -account <local-id> -output <new-file>, with no positional arguments")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := filepath.Abs(*output)
	if err != nil {
		return fmt.Errorf("resolve credential destination: %w", err)
	}
	if err := checkDestination(path); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	challenge, wait, err := start(ctx, *platform)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Authorization URL (render this URL as a QR code in your host UI):\n%s\nExpires: %s\nWaiting for authorization; Ctrl-C cancels.\n", challenge.URL, challenge.ExpiresAt.Format(time.RFC3339)); err != nil {
		return err
	}
	c, err := wait(ctx)
	if err != nil {
		return err
	}
	storedPlatform := *platform
	if storedPlatform == "lark" {
		storedPlatform = "feishu"
	}
	// Wait returns candidate credentials. Honor cancellation at the caller's
	// persistence boundary before starting the file commit.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := saveNew(path, savedAccount{Platform: storedPlatform, AccountID: *account, Credentials: c}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Saved credentials for %q to %q. No connection or message was started.\n", *account, path)
	return err
}

// Check before starting remote registration, then use O_EXCL again when saving
// to protect an existing account even if the destination appears during login.
func checkDestination(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return errors.New("credential destination already exists; choose a new file")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect credential destination: %w", err)
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("credential directory must already exist: %w", err)
	}
	if !info.IsDir() {
		return errors.New("credential parent is not a directory")
	}
	probe, err := os.CreateTemp(dir, ".channel-login-check-*")
	if err != nil {
		return fmt.Errorf("credential directory is not writable: %w", err)
	}
	if err := errors.Join(probe.Close(), os.Remove(probe.Name())); err != nil {
		return fmt.Errorf("check credential directory: %w", err)
	}
	return nil
}

func saveNew(path string, account savedAccount) (err error) {
	data, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create new credential file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			err = errors.Join(err, os.Remove(path))
		}
	}()
	if n, writeErr := f.Write(data); writeErr != nil {
		return fmt.Errorf("write credentials: %w", writeErr)
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync credentials: %w", err)
	}
	err = f.Close()
	closed = true
	return err
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, begin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
