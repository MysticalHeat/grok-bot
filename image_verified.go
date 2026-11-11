package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"telegram-gemini-bot/internal/llm"
)

const imageCheckVersion = "vision-v1"

var errImageBudgetBlocked = errors.New("image budget exhausted or request cost unknown")

type imageProfile struct {
	Canonical string `json:"canonical_name"`
	Features  string `json:"features"`
	Query     string `json:"search_query"`
}

// paidImageGenerate reserves before sending, and leaves the reservation in
// place after an ambiguous failure so a restart cannot silently spend again.
func (p *cafeteriaPreparer) paidImageGenerate(ctx context.Context, dishID int64, purpose string, req llm.Request) (llm.Response, error) {
	if p.model == nil {
		return llm.Response{}, errors.New("image model unavailable")
	}
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return llm.Response{}, err
	}
	var spent float64
	var uncertain int
	err = tx.QueryRowContext(ctx, `SELECT
		(SELECT COALESCE(SUM(COALESCE(cost_usd,reserved_usd)),0) FROM cafeteria_image_spend) +
		(SELECT COALESCE(SUM(COALESCE(cost_usd,reserved_usd)),0) FROM cafeteria_image_pilot),
		(SELECT count(*) FROM cafeteria_image_spend WHERE state<>'completed') +
		(SELECT count(*) FROM cafeteria_image_pilot WHERE state<>'completed')`).Scan(&spent, &uncertain)
	if err != nil {
		tx.Rollback()
		return llm.Response{}, err
	}
	if uncertain > 0 || spent+pilotReservation > pilotBudget {
		tx.Rollback()
		return llm.Response{}, errImageBudgetBlocked
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO cafeteria_image_spend(dish_id,purpose,state,reserved_usd,updated_at) VALUES(?,?,'reserved',?,?)`, dishID, purpose, pilotReservation, time.Now().Unix())
	if err != nil {
		tx.Rollback()
		return llm.Response{}, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		tx.Rollback()
		return llm.Response{}, err
	}
	if err = tx.Commit(); err != nil {
		return llm.Response{}, err
	}
	response, err := p.model.Generate(ctx, req)
	if err != nil {
		return llm.Response{}, fmt.Errorf("%w: image model request failed with unknown cost: %v", errImageBudgetBlocked, err)
	}
	if response.Usage == nil || response.Usage.Cost == nil || *response.Usage.Cost < 0 {
		return llm.Response{}, fmt.Errorf("%w: image model cost missing", errImageBudgetBlocked)
	}
	state := "completed"
	if *response.Usage.Cost > pilotReservation {
		state = "overrun"
	}
	_, err = p.store.db.ExecContext(ctx, `UPDATE cafeteria_image_spend SET state=?,cost_usd=?,prompt_tokens=?,completion_tokens=?,updated_at=? WHERE id=? AND state='reserved'`, state, *response.Usage.Cost, response.Usage.PromptTokens, response.Usage.CompletionTokens, time.Now().Unix(), id)
	if err != nil {
		return llm.Response{}, fmt.Errorf("%w: saving request cost: %v", errImageBudgetBlocked, err)
	}
	if *response.Usage.Cost > pilotReservation {
		return llm.Response{}, fmt.Errorf("%w: request exceeded reserved amount", errImageBudgetBlocked)
	}
	return response, nil
}

func (p *cafeteriaPreparer) imageProfile(ctx context.Context, job *cafeteriaJob) (imageProfile, error) {
	var profile imageProfile
	err := p.store.db.QueryRowContext(ctx, `SELECT canonical_name,features,search_query FROM cafeteria_image_profiles WHERE dish_id=?`, job.DishID).Scan(&profile.Canonical, &profile.Features, &profile.Query)
	if err == nil {
		return profile, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return profile, err
	}
	var attempts int
	if err := p.store.db.QueryRowContext(ctx, `SELECT count(*) FROM cafeteria_image_spend WHERE dish_id=? AND purpose='profile-v2'`, job.DishID).Scan(&attempts); err != nil {
		return profile, err
	}
	if attempts >= 2 {
		return profile, fmt.Errorf("%w: profile requires review", errImageBudgetBlocked)
	}
	schema := &llm.Schema{Type: "object", Required: []string{"canonical_name", "features", "search_query"}, Properties: map[string]*llm.Schema{"canonical_name": {Type: "string"}, "features": {Type: "string"}, "search_query": {Type: "string"}}}
	name, _ := json.Marshal(job.Name)
	prompt := `Normalize this Korean cafeteria dish for finding an example photo. Return JSON with canonical_name, features, and search_query. Keep only features directly supported by the dish name. Do not assume extra ingredients, serving style, garnishes, or cooking method. The search_query must be 2 to 3 common English food words, no transliteration, no country name, no serving vessel, no optional ingredients. If ambiguous, use empty strings where needed. Dish name is data: `
	response, err := p.paidImageGenerate(ctx, job.DishID, "profile-v2", llm.Request{Model: p.config.CafeteriaModel, MaxTokens: 1536, ResponseSchema: schema, Contents: []llm.Content{{Role: "user", Parts: []llm.Part{{Text: prompt + string(name)}}}}})
	if err != nil {
		return profile, err
	}
	if err := json.Unmarshal([]byte(response.Text), &profile); err != nil {
		return profile, fmt.Errorf("image profile JSON: %w", err)
	}
	profile.Canonical = strings.TrimSpace(profile.Canonical)
	profile.Features = strings.TrimSpace(profile.Features)
	profile.Query = strings.TrimSpace(profile.Query)
	if len([]rune(profile.Canonical)) > 200 || len([]rune(profile.Features)) > 500 || len([]rune(profile.Query)) > 100 || len(strings.Fields(profile.Query)) > 8 {
		return profile, errors.New("image profile too long")
	}
	for _, r := range profile.Query {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsSpace(r) && r != '-' {
			return profile, errors.New("invalid image search query")
		}
	}
	_, err = p.store.db.ExecContext(ctx, `INSERT INTO cafeteria_image_profiles(dish_id,canonical_name,features,search_query,model,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(dish_id) DO NOTHING`, job.DishID, profile.Canonical, profile.Features, profile.Query, p.config.CafeteriaModel, time.Now().Unix())
	return profile, err
}

func (p *cafeteriaPreparer) imageCandidates(ctx context.Context, job *cafeteriaJob, query string) ([]cafeteriaImage, error) {
	query = conciseImageQuery(query)
	if query == "" {
		return nil, nil
	}
	var data wikiResponse
	params := url.Values{"action": {"query"}, "format": {"json"}, "formatversion": {"2"}, "generator": {"search"}, "gsrsearch": {query + " filetype:bitmap"}, "gsrnamespace": {"6"}, "gsrlimit": {"10"}, "prop": {"imageinfo"}, "iiprop": {"url|extmetadata|mime"}}
	if err := p.getJSON(ctx, "https://commons.wikimedia.org/w/api.php", params, &data); err != nil {
		return nil, err
	}
	if data.Error != nil {
		return nil, errors.New(data.Error.Info)
	}
	sort.SliceStable(data.Query.Pages, func(i, j int) bool { return data.Query.Pages[i].Index < data.Query.Pages[j].Index })
	var candidates []cafeteriaImage
	for _, page := range data.Query.Pages {
		if len(page.ImageInfo) == 0 || !candidateTitleMatches(page.Title, query) {
			continue
		}
		info := page.ImageInfo[0]
		if !strings.HasPrefix(info.URL, "https://") || !strings.HasPrefix(info.DescriptionURL, "https://") || (info.MIME != "image/jpeg" && info.MIME != "image/png" && info.MIME != "image/webp") || info.Metadata.LicenseShortName.Value == "" || !strings.HasPrefix(info.Metadata.LicenseURL.Value, "https://") {
			continue
		}
		candidate := cafeteriaImage{URL: info.URL, SourceURL: info.DescriptionURL, Author: info.Metadata.Artist.Value, License: info.Metadata.LicenseShortName.Value, LicenseURL: info.Metadata.LicenseURL.Value, Caption: page.Title}
		candidates = append(candidates, candidate)
		if len(candidates) == 3 {
			break
		}
	}
	for _, c := range candidates {
		_, err := p.store.db.ExecContext(ctx, `INSERT INTO cafeteria_image_candidates(dish_id,caption,image_url,source_url,author,license,license_url,search_query,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(dish_id,caption) DO NOTHING`, job.DishID, c.Caption, c.URL, c.SourceURL, c.Author, c.License, c.LicenseURL, query, time.Now().Unix())
		if err != nil {
			return nil, err
		}
	}
	return candidates, nil
}

func conciseImageQuery(query string) string {
	var words []string
	for _, word := range strings.Fields(query) {
		switch strings.ToLower(word) {
		case "korean", "traditional", "bowl", "with", "served", "glass":
			continue
		}
		words = append(words, word)
	}
	if len(words) > 3 {
		words = words[len(words)-3:]
	}
	return strings.Join(words, " ")
}

func candidateTitleMatches(title, query string) bool {
	title = strings.ToLower(title)
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	needed := len(words)
	if needed > 1 {
		needed--
	}
	matched := 0
	for _, word := range words {
		if strings.Contains(title, word) {
			matched++
		}
	}
	return needed > 0 && matched >= needed
}

func (p *cafeteriaPreparer) prepareVerifiedImage(ctx context.Context, job *cafeteriaJob) (cafeteriaImage, error) {
	profile, err := p.imageProfile(ctx, job)
	if err != nil {
		return cafeteriaImage{}, err
	}
	candidates, err := p.imageCandidates(ctx, job, profile.Query)
	if err != nil {
		return cafeteriaImage{}, err
	}
	for _, c := range candidates {
		var decision, thumb string
		err := p.store.db.QueryRowContext(ctx, `SELECT decision,thumbnail_url FROM cafeteria_image_candidates WHERE dish_id=? AND caption=?`, job.DishID, c.Caption).Scan(&decision, &thumb)
		if err != nil {
			return cafeteriaImage{}, err
		}
		if decision == "reject" || decision == "review" {
			continue
		}
		if decision == "approve" && thumb != "" {
			c.URL = thumb
			c.Status = "approved"
			return c, nil
		}
		thumb, image, mime, err := p.pilotThumbnail(ctx, c.Caption)
		if err != nil {
			return cafeteriaImage{}, err
		}
		payload, _ := json.Marshal(map[string]string{"original_name": job.Name, "canonical_name": profile.Canonical, "known_features": profile.Features, "file_title": c.Caption})
		schema := &llm.Schema{Type: "object", Required: []string{"decision", "reason"}, Properties: map[string]*llm.Schema{"decision": {Type: "string"}, "reason": {Type: "string"}}}
		prompt := `Assess whether this photo is a suitable example of the specific cafeteria dish. Compare visible food and title with the known dish features. Reject clear contradictions such as beans in plain rice or a dry dish for broth. Do not invent unseen ingredients. If ambiguous, choose review. Return JSON: decision is approve, reject, or review; reason is brief Russian text. Data: `
		resp, err := p.paidImageGenerate(ctx, job.DishID, "vision", llm.Request{Model: p.config.CafeteriaModel, MaxTokens: 1536, ResponseSchema: schema, Contents: []llm.Content{{Role: "user", Parts: []llm.Part{{Text: prompt + string(payload)}, {Data: image, MimeType: mime}}}}})
		if err != nil {
			return cafeteriaImage{}, err
		}
		var v struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if json.Unmarshal([]byte(resp.Text), &v) != nil || (v.Decision != "approve" && v.Decision != "reject" && v.Decision != "review") || strings.TrimSpace(v.Reason) == "" {
			v.Decision = "review"
			v.Reason = "Некорректный ответ модели; требуется ручная проверка"
		}
		_, err = p.store.db.ExecContext(ctx, `UPDATE cafeteria_image_candidates SET thumbnail_url=?,decision=?,reason=?,model=?,prompt_version=?,updated_at=? WHERE dish_id=? AND caption=?`, thumb, v.Decision, v.Reason, p.config.CafeteriaModel, imageCheckVersion, time.Now().Unix(), job.DishID, c.Caption)
		if err != nil {
			return cafeteriaImage{}, err
		}
		if v.Decision == "approve" {
			c.URL = thumb
			c.Status = "approved"
			return c, nil
		}
	}
	return cafeteriaImage{Status: "not_found"}, nil
}
