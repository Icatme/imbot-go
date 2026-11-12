package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/channels/onboarding"
)

func args(platform, path string) []string {
	return []string{"-platform", platform, "-account", "support", "-output", path}
}

func TestValidationPrecedesRemoteAuthorization(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.json")
	if err := os.WriteFile(existing, []byte("previous credentials"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]string{
		nil,
		args("unknown", filepath.Join(dir, "unknown.json")),
		{"-platform", "dingtalk", "-output", filepath.Join(dir, "no-account.json")},
		{"-platform", "dingtalk", "-account", "support"},
		append(args("dingtalk", filepath.Join(dir, "extra.json")), "unexpected"),
		args("dingtalk", existing),
		args("feishu", dir),
		args("lark", filepath.Join(dir, "missing", "credentials.json")),
	} {
		t.Run(strings.Join(input, " "), func(t *testing.T) {
			err := run(context.Background(), input, io.Discard, func(context.Context, string) (onboarding.Challenge, func(context.Context) (credentials, error), error) {
				t.Fatal("remote authorization started before validation")
				return onboarding.Challenge{}, nil, nil
			})
			if err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "previous credentials" {
		t.Fatalf("existing credentials changed: %s %v", data, err)
	}
}

func TestLoginSavesCredentialsWithoutPrintingThem(t *testing.T) {
	for _, platform := range []string{"dingtalk", "feishu", "lark", "wecom", "qqbot"} {
		t.Run(platform, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "credentials.json")
			var out bytes.Buffer
			want := credentials{ClientID: "app", ClientSecret: "top-secret"}
			switch platform {
			case "feishu", "lark":
				want = credentials{AppID: "app", AppSecret: "top-secret", Domain: platform, OpenID: "user"}
			case "wecom":
				want = credentials{BotID: "bot", Secret: "top-secret"}
			case "qqbot":
				want = credentials{AppID: "app", ClientSecret: "top-secret", UserOpenID: "user"}
			}
			err := run(context.Background(), args(platform, path), &out, func(_ context.Context, got string) (onboarding.Challenge, func(context.Context) (credentials, error), error) {
				if got != platform {
					t.Fatalf("platform = %s, want %s", got, platform)
				}
				return onboarding.Challenge{URL: "https://example.test/authorize", ExpiresAt: time.Now().Add(time.Minute)}, func(context.Context) (credentials, error) { return want, nil }, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "top-secret") || !strings.Contains(out.String(), "https://example.test/authorize") {
				t.Fatalf("unexpected output: %s", &out)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved savedAccount
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			wantPlatform := platform
			if platform == "lark" {
				wantPlatform = "feishu"
			}
			if saved.Platform != wantPlatform || saved.AccountID != "support" || saved.Credentials != want {
				t.Fatal("saved account identity or credentials changed")
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("file permissions = %v", info.Mode())
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("preflight left extra files: %v %v", entries, err)
			}
		})
	}
}

func TestDestinationCreatedDuringLoginIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	err := run(context.Background(), args("dingtalk", path), io.Discard, func(context.Context, string) (onboarding.Challenge, func(context.Context) (credentials, error), error) {
		return onboarding.Challenge{}, func(context.Context) (credentials, error) {
			if err := os.WriteFile(path, []byte("other account"), 0600); err != nil {
				t.Fatal(err)
			}
			return credentials{ClientID: "new", ClientSecret: "new-secret"}, nil
		}, nil
	})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected exclusive creation failure, got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "other account" {
		t.Fatalf("overwrote another account: %s %v", data, err)
	}
}

func TestCancellationDoesNotStartOrSaveRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, args("dingtalk", path), io.Discard, func(context.Context, string) (onboarding.Challenge, func(context.Context) (credentials, error), error) {
		t.Fatal("authorization started after cancellation")
		return onboarding.Challenge{}, nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	err = run(ctx, args("dingtalk", path), io.Discard, func(context.Context, string) (onboarding.Challenge, func(context.Context) (credentials, error), error) {
		cancel()
		return onboarding.Challenge{}, func(ctx context.Context) (credentials, error) { return credentials{}, ctx.Err() }, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled login wrote a credential file: %v", err)
	}
}

func TestFailedAuthorizationDoesNotSaveCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	err := run(context.Background(), args("feishu", path), io.Discard, func(context.Context, string) (onboarding.Challenge, func(context.Context) (credentials, error), error) {
		return onboarding.Challenge{}, func(context.Context) (credentials, error) { return credentials{}, onboarding.ErrDenied }, nil
	})
	if !errors.Is(err, onboarding.ErrDenied) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed login wrote a credential file: %v", err)
	}
}

func TestCancellationAtCredentialHandoffDoesNotSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := run(ctx, args("qqbot", path), io.Discard, func(context.Context, string) (onboarding.Challenge, func(context.Context) (credentials, error), error) {
		return onboarding.Challenge{}, func(context.Context) (credentials, error) {
			// Simulate Ctrl-C after Wait accepted a completed response but
			// before the caller commits its returned candidate credentials.
			cancel()
			return credentials{AppID: "new-app", ClientSecret: "new-secret"}, nil
		}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled credential handoff returned %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled handoff saved credentials: %v", err)
	}
}
