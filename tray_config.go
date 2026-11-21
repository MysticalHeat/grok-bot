package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"telegram-gemini-bot/internal/llm"
)

func configureTrayPhotos(ctx context.Context, cfg Config, store *SQLiteStore, bot *tgbotapi.BotAPI) (*trayPhotos, error) {
	if !getEnvAsBool("TRAY_PHOTOS_ENABLED", false) {
		return nil, nil
	}
	p := &trayPhotos{store: store, now: time.Now, admins: map[int64]bool{}, uploadSlots: make(chan struct{}, 2)}
	for _, id := range getEnvAsInt64List("TRAY_PHOTO_ADMIN_IDS") {
		if id > 0 {
			p.admins[id] = true
		}
	}
	if len(p.admins) == 0 {
		return nil, errors.New("TRAY_PHOTO_ADMIN_IDS must contain at least one Telegram user ID")
	}
	p.frames = strings.TrimSpace(os.Getenv("TRAY_FRAME_DIR"))
	if p.frames == "" {
		p.frames = cfg.SQLitePath + ".tray-frames"
	}
	if err := os.MkdirAll(p.frames, 0700); err != nil {
		return nil, err
	}
	if stat, err := os.Lstat(p.frames); err != nil {
		return nil, err
	} else if !stat.IsDir() || stat.Mode().Perm()&0077 != 0 {
		return nil, errors.New("TRAY_FRAME_DIR must be a private directory (0700)")
	}
	p.frames = filepath.Clean(p.frames)
	p.model = openRouterModel(os.Getenv("TRAY_PHOTO_MODEL"))
	p.dailyCalls = getEnvAsInt("TRAY_PHOTO_DAILY_AI_CALLS", 20)
	if p.model != "" {
		if p.dailyCalls < 1 || p.dailyCalls > 1000 {
			return nil, errors.New("TRAY_PHOTO_DAILY_AI_CALLS must be between 1 and 1000")
		}
		client, err := llm.NewClient(ctx, cafeteriaLLMConfig(cfg))
		if err != nil {
			return nil, err
		}
		p.ai = client
	}
	p.notify = func(admin, id int64) error {
		msg := tgbotapi.NewMessage(admin, fmt.Sprintf("Фото подноса №%d готово к проверке.", id))
		link := fmt.Sprintf("https://t.me/%s?startapp=tray_%d", bot.Self.UserName, id)
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("Открыть заявку", link),
		))
		_, err := bot.Send(msg)
		return err
	}
	return p, nil
}
