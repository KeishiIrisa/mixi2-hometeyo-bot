package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log"
	"os"

	"github.com/mixigroup/mixi2-application-sdk-go/auth"
	"github.com/mixigroup/mixi2-application-sdk-go/event/webhook"
	constv1 "github.com/mixigroup/mixi2-application-sdk-go/gen/go/social/mixi/application/const/v1"
	modelv1 "github.com/mixigroup/mixi2-application-sdk-go/gen/go/social/mixi/application/model/v1"
	application_apiv1 "github.com/mixigroup/mixi2-application-sdk-go/gen/go/social/mixi/application/service/application_api/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type MyHandler struct {
	gemini        *geminiClient
	client        application_apiv1.ApplicationServiceClient
	authenticator auth.Authenticator
}

func (h *MyHandler) Handle(ctx context.Context, ev *modelv1.Event) error {
	postCreated := ev.GetPostCreatedEvent()
	if postCreated == nil {
		return nil
	}

	// メンションされた投稿かどうかをチェック
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

	imageURL := ""
	if mediaList := post.GetPostMediaList(); len(mediaList) > 0 {
		if media := mediaList[0]; media != nil {
			if img := media.GetImage(); img != nil {
				imageURL = img.GetLargeImageUrl()
				if imageURL == "" {
					imageURL = img.GetSmallImageUrl()
				}
			}
		}
	}

	compliment, err := h.gemini.GenerateCompliment(ctx, post.GetText(), imageURL)
	if err != nil {
		log.Printf("failed to generate compliment: %v", err)
		return nil
	}

	inReplyTo := post.GetPostId()

	publishingType := constv1.PostPublishingType_POST_PUBLISHING_TYPE_UNSPECIFIED

	req := &application_apiv1.CreatePostRequest{
		Text:            compliment,
		InReplyToPostId: &inReplyTo,
		PublishingType:  &publishingType,
	}

	authCtx, err := h.authenticator.AuthorizedContext(ctx)
	if err != nil {
		log.Printf("failed to attach auth context: %v", err)
		return nil
	}

	resp, err := h.client.CreatePost(authCtx, req)
	if err != nil {
		log.Printf("failed to create reply post: %v", err)
		return nil
	}

	log.Printf("Created reply post: %s", resp.GetPost().GetPostId())

	// スタンプ一覧を取得し、Gemini に投稿に合うスタンプを選ばせる
	stampsResp, err := h.client.GetStamps(authCtx, &application_apiv1.GetStampsRequest{
		OfficialStampLanguage: constv1.LanguageCode_LANGUAGE_CODE_JP.Enum(),
	})
	if err != nil {
		log.Printf("failed to get stamps: %v", err)
		return nil
	}

	var stampOptions []StampOption
	for _, set := range stampsResp.GetOfficialStampSets() {
		for _, s := range set.GetStamps() {
			if s.GetStampId() != "" {
				stampOptions = append(stampOptions, StampOption{
					StampId:    s.GetStampId(),
					SearchTags: s.GetSearchTags(),
				})
			}
		}
	}

	var stampID string
	if len(stampOptions) > 0 {
		stampID, err = h.gemini.SelectStamp(ctx, post.GetText(), imageURL, stampOptions)
		if err != nil {
			log.Printf("failed to select stamp: %v", err)
		}
	}

	if stampID != "" {
		_, err = h.client.AddStampToPost(authCtx, &application_apiv1.AddStampToPostRequest{
			PostId:  post.GetPostId(),
			StampId: stampID,
		})
		if err != nil {
			log.Printf("failed to add stamp to post: %v", err)
		} else {
			log.Printf("Added stamp %s to post %s", stampID, post.GetPostId())
		}
	}
	return nil
}

func main() {
	publicKeyBase64 := os.Getenv("SIGNATURE_PUBLIC_KEY")
	if publicKeyBase64 == "" {
		log.Fatal("SIGNATURE_PUBLIC_KEY environment variable is not set")
	}
	publicKey, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil {
		log.Fatalf("failed to decode SIGNATURE_PUBLIC_KEY: %v", err)
	}

	clientID := os.Getenv("CLIENT_ID")
	clientSecret := os.Getenv("CLIENT_SECRET")
	tokenURL := os.Getenv("TOKEN_URL")
	apiAddress := os.Getenv("API_ADDRESS")

	if clientID == "" || clientSecret == "" || tokenURL == "" || apiAddress == "" {
		log.Fatal("CLIENT_ID, CLIENT_SECRET, TOKEN_URL, and API_ADDRESS must be set")
	}

	authenticator, err := auth.NewAuthenticator(
		clientID,
		clientSecret,
		tokenURL,
	)
	if err != nil {
		log.Fatal(err)
	}

	conn, err := grpc.NewClient(
		apiAddress,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	apiClient := application_apiv1.NewApplicationServiceClient(conn)

	ctx := context.Background()
	geminiClient, err := newGeminiClientFromEnv(ctx)
	if err != nil {
		log.Fatal(err)
	}

	handler := &MyHandler{
		gemini:        geminiClient,
		client:        apiClient,
		authenticator: authenticator,
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := webhook.NewServer(
		":"+port,
		ed25519.PublicKey(publicKey),
		handler,
	)

	if err := server.Start(); err != nil {
		log.Fatal(err)
	}

}
