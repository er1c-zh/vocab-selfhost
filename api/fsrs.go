package main

import (
	"math"
	"time"
)

// FSRS-4.5 default parameters, using the published DSR formulas.
var fsrsW = [...]float64{
	0.4872, 1.4003, 3.7145, 13.8206, 5.1618, 1.2298,
	0.8975, 0.031, 1.6474, 0.1367, 1.0461, 2.1072,
	0.0793, 0.3246, 1.587, 0.2272, 2.8755,
}

const desiredRetention = 0.9

func applyFSRS(card Card, rating int, now time.Time) Card {
	days := 0.0
	if !card.LastReview.IsZero() {
		days = math.Max(0, now.Sub(card.LastReview).Hours()/24)
	}

	if card.Stability <= 0 {
		card.Stability = fsrsW[rating-1]
		card.Difficulty = initialDifficulty(rating)
	} else {
		r := math.Pow(1+(19.0/81.0)*days/card.Stability, -0.5)
		d0 := initialDifficulty(3)
		card.Difficulty = clamp(fsrsW[7]*d0+(1-fsrsW[7])*(card.Difficulty-fsrsW[6]*float64(rating-3)), 1, 10)
		if rating == 1 {
			card.Stability = fsrsW[11] * math.Pow(card.Difficulty, -fsrsW[12]) *
				(math.Pow(card.Stability+1, fsrsW[13]) - 1) * math.Exp(fsrsW[14]*(1-r))
		} else {
			gradeBoost := 1.0
			if rating == 2 {
				gradeBoost = fsrsW[15]
			} else if rating == 4 {
				gradeBoost = fsrsW[16]
			}
			card.Stability *= 1 + math.Exp(fsrsW[8])*(11-card.Difficulty)*math.Pow(card.Stability, -fsrsW[9])*(math.Exp(fsrsW[10]*(1-r))-1)*gradeBoost
		}
	}

	if rating == 1 {
		card.Lapses++
	}
	card.Reps++
	card.State = "review"
	card.LastReview = now.UTC()
	interval := card.Stability / (19.0 / 81.0) * (math.Pow(desiredRetention, -2) - 1)
	interval = math.Max(1, math.Min(36500, interval))
	card.DueAt = now.UTC().Add(time.Duration(math.Round(interval*24) * float64(time.Hour)))
	card.UpdatedAt = now.UTC()
	return card
}

func initialDifficulty(rating int) float64 {
	return clamp(fsrsW[4]-math.Exp(fsrsW[5]*float64(rating-1))+1, 1, 10)
}

func clamp(v, min, max float64) float64 {
	return math.Max(min, math.Min(max, v))
}
