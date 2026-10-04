package main

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestGIFOf(t *testing.T) {
	cases := []struct {
		name     string
		msg      models.Message
		wantID   string
		wantAnim bool
	}{
		{name: "анимация", msg: models.Message{Animation: &models.Animation{FileID: "a"}}, wantID: "a", wantAnim: true},
		{name: "GIF файлом", msg: models.Message{Document: &models.Document{FileID: "d", MimeType: "image/gif"}}, wantID: "d"},
		{name: "mp4 файлом", msg: models.Message{Document: &models.Document{FileID: "m", MimeType: "video/mp4"}}, wantID: "m"},
		{name: "видео", msg: models.Message{Video: &models.Video{FileID: "v"}}, wantID: "v"},
		{name: "pdf не GIF", msg: models.Message{Document: &models.Document{FileID: "p", MimeType: "application/pdf"}}},
		{name: "ответ на анимацию",
			msg:    models.Message{Text: "/gif disk", ReplyToMessage: &models.Message{Animation: &models.Animation{FileID: "r"}}},
			wantID: "r", wantAnim: true},
		{name: "ответ на GIF файлом",
			msg:    models.Message{Text: "/gif disk", ReplyToMessage: &models.Message{Document: &models.Document{FileID: "rd", MimeType: "image/gif"}}},
			wantID: "rd"},
		{name: "текст", msg: models.Message{Text: "/status"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, anim := gifOf(&c.msg)
			if id != c.wantID || anim != c.wantAnim {
				t.Fatalf("gifOf = %q, %v; нужно %q, %v", id, anim, c.wantID, c.wantAnim)
			}
		})
	}
}
