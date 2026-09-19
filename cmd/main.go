//go:generate go -C ../scripts run . res

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"whatsrook"
	"whatsrook/cmd/updater"
	"whatsrook/util"
	"whatsrook/util/cache"
	"whatsrook/util/logger"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			crashPath := util.RecordCrash(r, "top-level process panic")
			logger.Error("Fatal runtime crash", "panic", r, "crash_log", crashPath)
			os.Exit(1)
		}
	}()

	args := parseCLIArgs()

	if args.Version {
		v := Version
		if v == "dev" || v == "" {
			v = updater.GetBinaryVersion()
		}
		fmt.Println(v)
		return
	}

	if args.Verbose {
		logger.SetVerbose(true)
	}

	if args.Update {
		handleUpdate(args.UpdateOp)
		return
	}

	if args.AutoUpdate {
		handleAutoUpdateCLI(args.AutoUpdateVal)
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if updater.GetStoredAutoUpdate() && os.Getenv("_WHATSRROK_AUTOUPDATE_RESTARTED") != "1" {
		ctxAuto, cancelAuto := context.WithTimeout(ctx, 45*time.Second)
		updated, err := updater.PerformAutoUpdate(ctxAuto, os.Stdout)
		cancelAuto()
		if err != nil {
			logger.Warn("auto-update check failed", "err", err)
		} else if updated {
			fmt.Println("==> Auto-update applied successfully! Restarting WhatsRook...")
			_ = os.Setenv("_WHATSRROK_AUTOUPDATE_RESTARTED", "1")
			err := updater.RestartProcess()
			logger.Error("failed to restart process after auto-update", "err", err)
			return
		}
	}

	if args.Logout {
		handleLogoutCLI(ctx, args)
		return
	}

	cache.Init(10000)
	defer func() {
		_ = cache.Close()
	}()

	clientType, ok := whatsrook.ParseClientType(args.Client)
	if !ok {
		clientType = whatsrook.ClientChrome
	}

	bot := NewBot(BotConfig{
		Session:         args.Session,
		Pair:            args.Auth == "pair",
		QRCode:          args.Auth == "qr",
		Logout:          args.Logout,
		ClientType:      clientType,
		Business:        args.Business,
		Database:        args.Database,
		Verbose:         args.Verbose,
		WSPort:          0,
		AsyncMessageAck: true,
	})

	err := bot.Start(ctx)
	if err != nil {
		logger.Error("bot execution failure", "err", err)
		os.Exit(1)
	}
}

func handleUpdate(op string) {
	ctx := context.Background()

	var targetChannel string
	switch op {
	case "stable", "beta":
		targetChannel = op
		current := updater.GetStoredChannel()
		if op != current {
			fmt.Printf("==> Switching release channel: %s -> %s\n", current, op)
			if err := updater.SetStoredChannel(op); err != nil {
				logger.Error("failed to set release channel", "err", err)
				os.Exit(1)
			}
		} else {
			fmt.Printf("==> Already tracking channel: %s\n", current)
		}
	default:
		if updater.CurrentIsBeta() {
			targetChannel = "beta"
		} else {
			targetChannel = "stable"
		}
	}

	if op == "check" {
		up := updater.New(updater.Options{
			Out:     os.Stdout,
			Channel: targetChannel,
		})
		if _, err := up.Check(ctx); err != nil {
			logger.Error("update check failed", "err", err)
			os.Exit(1)
		}
		return
	}

	up := updater.New(updater.Options{
		Out:     os.Stdout,
		Channel: targetChannel,
	})

	res, err := up.Upgrade(ctx, targetChannel == "beta")
	if err != nil {
		logger.Error("upgrade procedure failed", "err", err)
		os.Exit(1)
	}

	if res.Updated {
		fmt.Println("==> Upgrade complete!")
	}
}

func handleAutoUpdateCLI(val string) {
	switch val {
	case "on":
		if err := updater.SetStoredAutoUpdate(true); err != nil {
			logger.Error("failed to enable auto-update", "err", err)
			os.Exit(1)
		}
		fmt.Println("==> Auto-update enabled. WhatsRook will automatically check and apply updates on startup.")
	case "off":
		if err := updater.SetStoredAutoUpdate(false); err != nil {
			logger.Error("failed to disable auto-update", "err", err)
			os.Exit(1)
		}
		fmt.Println("==> Auto-update disabled.")
	default:
		if updater.GetStoredAutoUpdate() {
			fmt.Println("Auto-update is currently enabled (ON).")
		} else {
			fmt.Println("Auto-update is currently disabled (OFF).")
		}
	}
}
