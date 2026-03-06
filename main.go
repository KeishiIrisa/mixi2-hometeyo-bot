package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log"
	"os"

	constv1 "github.com/mixigroup/mixi2-application-sdk-go/gen/go/social/mixi/application/const/v1"
	"github.com/mixigroup/mixi2-application-sdk-go/event/webhook"
	modelv1 "github.com/mixigroup/mixi2-application-sdk-go/gen/go/social/mixi/application/model/v1"
)

type MyHandler struct{}

func (h *MyHandler) Handle(ctx context.Context, ev *modelv1.Event) error {
	postCreated := ev.GetPostCreatedEvent()
	if postCreated == nil {
		return nil
	}

	isMention := false
	for _, reason := range postCreated.GetEventReasonList() {
		if reason == constv1.EventReason_EVENT_REASON_POST_MENTIONED {
			isMention = true
			break
		}
	}
	if !isMention {
		return nil
	}

	post := postCreated.GetPost()
	if post == nil {
		return nil
	}

	log.Printf("Echoing mentioned post: %s", post.GetText())
	return nil
}

func main() {
	publicKeyBase64 := os.Getenv("SIGNATURE_PUBLIC_KEY")
	publicKey, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil {
		log.Fatal(err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := webhook.NewServer(
		":"+port,
		ed25519.PublicKey(publicKey),
		&MyHandler{},
	)

	if err := server.Start(); err != nil {
		log.Fatal(err)
	}

}
