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
	botUserID     string // ボット（アプリ）のユーザーID。リプライが自投稿へのリプライか判定するために使用
}

func (h *MyHandler) Handle(ctx context.Context, ev *modelv1.Event) error {
	postCreated := ev.GetPostCreatedEvent()
	if postCreated == nil {
		return nil
	}

	// メンションされた投稿、またはボットの投稿へのリプライかどうかをチェック
	// isReplyToBot: ユーザーがボットの投稿にリプライしてきた場合（会話の2ターン目以降）。このときは質問をせず短く締める。
	shouldRespond := false
	isReplyToBot := false
	for _, reason := range postCreated.GetEventReasonList() {
		if reason == constv1.EventReason_EVENT_REASON_POST_MENTIONED {
			shouldRespond = true
			break
		}
		if reason == constv1.EventReason_EVENT_REASON_POST_REPLY {
			// リプライの場合は、返信先がボット自身の投稿かどうかを確認する
			if h.botUserID != "" && h.isReplyToBotPost(ctx, postCreated.GetPost()) {
				shouldRespond = true
				isReplyToBot = true
				break
			}
		}
	}
	if !shouldRespond {
		return nil
	}

	post := postCreated.GetPost()
	if post == nil {
		return nil
	}

	authCtx, err := h.authenticator.AuthorizedContext(ctx)
	if err != nil {
		log.Printf("failed to attach auth context: %v", err)
		return nil
	}

	// 投稿者の表示名を取得（「〇〇さん」で会話するため）
	userName := ""
	if creatorID := post.GetCreatorId(); creatorID != "" {
		usersResp, err := h.client.GetUsers(authCtx, &application_apiv1.GetUsersRequest{
			UserIdList: []string{creatorID},
		})
		if err != nil {
			log.Printf("failed to get user info: %v", err)
		} else if len(usersResp.GetUsers()) > 0 {
			u := usersResp.GetUsers()[0]
			userName = u.GetDisplayName()
			if userName == "" {
				userName = u.GetName()
			}
		}
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

	compliment, err := h.gemini.GenerateCompliment(ctx, post.GetText(), imageURL, userName, isReplyToBot)
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

// isReplyToBotPost は、指定した投稿がボットの投稿へのリプライかどうかを判定する。
func (h *MyHandler) isReplyToBotPost(ctx context.Context, post *modelv1.Post) bool {
	inReplyToID := post.GetInReplyToPostId()
	if inReplyToID == "" {
		return false
	}
	authCtx, err := h.authenticator.AuthorizedContext(ctx)
	if err != nil {
		log.Printf("failed to attach auth context for GetPosts: %v", err)
		return false
	}
	resp, err := h.client.GetPosts(authCtx, &application_apiv1.GetPostsRequest{
		PostIdList: []string{inReplyToID},
	})
	if err != nil {
		log.Printf("failed to get parent post: %v", err)
		return false
	}
	if len(resp.GetPosts()) == 0 {
		return false
	}
	parentPost := resp.GetPosts()[0]
	return parentPost.GetCreatorId() == h.botUserID
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

	// ボットのユーザーID（リプライが自投稿へのリプライか判定するため）。未設定の場合はリプライには反応しない。
	botUserID := os.Getenv("APPLICATION_USER_ID")

	handler := &MyHandler{
		gemini:        geminiClient,
		client:        apiClient,
		authenticator: authenticator,
		botUserID:     botUserID,
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
