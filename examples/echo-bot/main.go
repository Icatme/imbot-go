// Echo bot example — receives messages and replies with "Echo: <text>".
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/Icatme/imbot-go"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bot := imbot.New(imbot.Options{
		OnQRURL: func(url string) {
			fmt.Printf("\nScan this URL in WeChat:\n%s\n\n", url)
		},
		OnError: func(err error) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		},
	})

	creds, err := bot.Login(ctx, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Logged in: %s (%s)\n", creds.AccountID, creds.UserID)

	var count atomic.Int64
	bot.Handle(imbot.MessageHandlerFunc(func(ctx context.Context, msg *imbot.IncomingMessage) imbot.MessageResult {
		current := count.Add(1)
		fmt.Printf("[%d] %s: %s\n", current, msg.UserID, msg.Text)

		_ = bot.SendTyping(ctx, msg.UserID)

		if err := bot.Reply(ctx, msg, fmt.Sprintf("Echo: %s", msg.Text)); err != nil {
			return imbot.RetryMessage(err)
		}
		return imbot.AckMessage()
	}))

	fmt.Println("Listening for messages (Ctrl+C to stop)")
	if err := bot.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Run error: %v\n", err)
	}
	fmt.Printf("Stopped. Processed %d messages.\n", count.Load())
}
