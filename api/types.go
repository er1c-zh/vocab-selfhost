package main

import "time"

type Card struct {
	ID             string    `json:"id"`
	Word           string    `json:"word"`
	IPA            string    `json:"ipa"`
	Definition     string    `json:"definition"`
	ContextText    string    `json:"contextText"`
	ContextMeaning string    `json:"contextMeaning"`
	Source         string    `json:"source"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	DueAt          time.Time `json:"dueAt"`
	State          string    `json:"state"`
	Difficulty     float64   `json:"difficulty"`
	Stability      float64   `json:"stability"`
	LastReview     time.Time `json:"lastReview,omitempty"`
	Reps           int       `json:"reps"`
	Lapses         int       `json:"lapses"`
}

type CardInput struct {
	ID             string `json:"id"`
	Word           string `json:"word"`
	IPA            string `json:"ipa"`
	Definition     string `json:"definition"`
	ContextText    string `json:"contextText"`
	ContextMeaning string `json:"contextMeaning"`
	Source         string `json:"source"`
}

type ReviewEvent struct {
	ID         string    `json:"id"`
	CardID     string    `json:"cardId"`
	Rating     int       `json:"rating"`
	ReviewedAt time.Time `json:"reviewedAt"`
}

type DictionaryEntry struct {
	Word       string `json:"word"`
	IPA        string `json:"ipa"`
	Definition string `json:"definition"`
	Locale     string `json:"locale"`
}

type BackupBundle struct {
	Version    int               `json:"version"`
	ExportedAt time.Time         `json:"exportedAt"`
	Cards      []Card            `json:"cards"`
	Reviews    []ReviewEvent     `json:"reviews"`
	Dictionary []DictionaryEntry `json:"dictionary"`
}

type ReviewRequest struct {
	ID         string    `json:"id"`
	Rating     int       `json:"rating"`
	ReviewedAt time.Time `json:"reviewedAt"`
}
