package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

func main() {
	if err := notify(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func notify() error {
	env, err := godotenv.Read("/tmp/grok-bot/.env")
	if err != nil {
		return fmt.Errorf("cannot read deployment environment")
	}
	token, chatID := env["TELEGRAM_BOT_TOKEN"], os.Getenv("TELEGRAM_NOTIFY_CHAT_ID")
	if token == "" || chatID == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN or TELEGRAM_NOTIFY_CHAT_ID is missing")
	}

	sha := os.Getenv("GITHUB_SHA")
	if len(sha) > 7 {
		sha = sha[:7]
	}
	runURL := fmt.Sprintf("%s/%s/actions/runs/%s", os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID"))
	message := fmt.Sprintf("✅ grok-bot успешно задеплоен\n\nВетка: %s\nКоммит: %s\nПроверки пройдены, сервис перезапущен.\n\nПодробности: %s", os.Getenv("GITHUB_REF_NAME"), sha, runURL)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm("https://api.telegram.org/bot"+token+"/sendMessage", url.Values{
		"chat_id": {chatID},
		"text":    {message},
	})
	if err != nil {
		// HTTP errors can contain the request URL, including the bot token.
		return fmt.Errorf("Telegram notification request failed")
	}
	defer resp.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.OK || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram notification failed (HTTP %s)", strings.TrimSpace(resp.Status))
	}
	return nil
}
