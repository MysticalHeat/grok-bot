package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web/menu/*
var menuAssets embed.FS

type menuDish struct {
	ID               int64    `json:"id"`
	PhotoID          string   `json:"photo_id,omitempty"`
	ImageKind        string   `json:"image_kind,omitempty"`
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	Course           string   `json:"course"`
	TranslatedName   string   `json:"translated_name"`
	ShortDescription string   `json:"short_description"`
	Description      string   `json:"description"`
	Ingredients      []string `json:"ingredients"`
	Sources          []string `json:"sources"`
	NeedsReview      bool     `json:"needs_review"`
	ImageURL         string   `json:"image_url"`
	ImageSource      string   `json:"image_source"`
	ImageAuthor      string   `json:"image_author"`
	ImageLicense     string   `json:"image_license"`
	ImageLicenseURL  string   `json:"image_license_url"`
	ImageStatus      string   `json:"image_status"`
}

func (s *SQLiteStore) menuForDate(ctx context.Context, date string) (map[string][]menuDish, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.meal_type, d.id,COALESCE(p.photo_id,''),d.name, d.kind, s.course,
		COALESCE(c.translated_name,''), COALESCE(c.short_description,''), COALESCE(c.description,''),
		COALESCE(c.typical_ingredients,'[]'), COALESCE(c.sources,'[]'), COALESCE(c.needs_review,1),
		COALESCE(i.image_url,''), COALESCE(i.source_url,''), COALESCE(i.author,''),
		COALESCE(i.license,''), COALESCE(i.license_url,''), COALESCE(i.status,'')
		FROM cafeteria_schedule s JOIN cafeteria_dishes d ON d.id=s.dish_id
		LEFT JOIN cafeteria_cards c ON c.dish_id=d.id LEFT JOIN cafeteria_images i ON i.dish_id=d.id AND i.status='approved'
		LEFT JOIN tray_published p ON p.dish_id=d.id
		WHERE s.service_date=? ORDER BY s.meal_type,s.position`, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	meals := make(map[string][]menuDish)
	for rows.Next() {
		var meal, ingredients, sources string
		var d menuDish
		if err := rows.Scan(&meal, &d.ID, &d.PhotoID, &d.Name, &d.Kind, &d.Course, &d.TranslatedName, &d.ShortDescription,
			&d.Description, &ingredients, &sources, &d.NeedsReview, &d.ImageURL, &d.ImageSource,
			&d.ImageAuthor, &d.ImageLicense, &d.ImageLicenseURL, &d.ImageStatus); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ingredients), &d.Ingredients)
		_ = json.Unmarshal([]byte(sources), &d.Sources)
		meals[meal] = append(meals[meal], d)
	}
	return meals, rows.Err()
}

// validTelegramInitData authenticates the entire query string, not initDataUnsafe.
func validTelegramInitData(raw, token string, now time.Time) bool {
	return telegramUserID(raw, token, now) != 0
}

func telegramUserID(raw, token string, now time.Time) int64 {
	if len(raw) == 0 || len(raw) > 16384 || token == "" {
		return 0
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return 0
	}
	for _, v := range values {
		if len(v) != 1 {
			return 0
		}
	}
	hash, err := hex.DecodeString(values.Get("hash"))
	if err != nil || len(hash) != sha256.Size {
		return 0
	}
	values.Del("hash")
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, key+"="+values.Get(key))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	_, _ = mac.Write([]byte(strings.Join(fields, "\n")))
	if !hmac.Equal(hash, mac.Sum(nil)) {
		return 0
	}
	stamp, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil || stamp > now.Unix()+30 || stamp < now.Add(-24*time.Hour).Unix() {
		return 0
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal([]byte(values.Get("user")), &user) != nil || user.ID <= 0 {
		return 0
	}
	return user.ID
}

func newMenuHandler(store *SQLiteStore, token string, photoOptions ...*trayPhotos) http.Handler {
	assets, err := fs.Sub(menuAssets, "web/menu")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	var photos *trayPhotos
	if len(photoOptions) > 0 {
		photos = photoOptions[0]
	}
	if photos != nil {
		photos.register(mux, token)
	}
	mux.HandleFunc("/api/menu", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		now := time.Now()
		if photos != nil {
			now = photos.now()
		}
		user := telegramUserID(r.Header.Get("X-Telegram-Init-Data"), token, now)
		if user == 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		today := now.In(seoulLocation).Format("2006-01-02")
		date := r.URL.Query().Get("date")
		if date == "" {
			date = today
		}
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil || parsed.Format("2006-01-02") != date {
			http.Error(w, "invalid date", http.StatusBadRequest)
			return
		}
		meals, err := store.menuForDate(r.Context(), date)
		if err != nil {
			log.Printf("menu query: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, dishes := range meals {
			for i := range dishes {
				d := &dishes[i]
				if photos == nil {
					d.PhotoID = ""
					continue
				}
				if d.PhotoID != "" {
					d.ImageURL, d.ImageKind, d.ImageStatus = "/api/tray-photo/"+d.PhotoID, "cafeteria", "approved"
					d.ImageSource, d.ImageAuthor, d.ImageLicense, d.ImageLicenseURL = "", "", "", ""
				}
			}
		}
		trayJSON(w, 200, struct {
			Date           string                `json:"date"`
			Today          string                `json:"today"`
			Meals          map[string][]menuDish `json:"meals"`
			PhotosEnabled  bool                  `json:"photos_enabled"`
			PhotoAIEnabled bool                  `json:"photo_ai_enabled"`
			PhotoAdmin     bool                  `json:"photo_admin"`
		}{date, today, meals, photos != nil, photos != nil && photos.ai != nil, photos != nil && photos.admins[user]})
	})
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return mux
}
