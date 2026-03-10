package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strings"

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
	isMention := false
	var threadPosts []*modelv1.Post
	for _, reason := range postCreated.GetEventReasonList() {
		if reason == constv1.EventReason_EVENT_REASON_POST_MENTIONED {
			shouldRespond = true
			isMention = true
			break
		}
		if reason == constv1.EventReason_EVENT_REASON_POST_REPLY {
			// リプライの場合は、スレッド全体をたどってボットの投稿が含まれているかを確認し、
			// ルートのユーザー投稿から現在の投稿までのポスト一覧を会話コンテキストとして取得する。
			isReply, posts := h.collectThreadIfReplyToBot(ctx, postCreated.GetPost())
			if h.botUserID != "" && isReply {
				shouldRespond = true
				isReplyToBot = true
				threadPosts = posts
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

	// ボットとの会話スレッド（ルート投稿から現在の投稿まで）があれば、Gemini に渡す用のテキストを構築する。
	var historyText string
	if len(threadPosts) > 0 {
		var b strings.Builder
		b.WriteString("これまでの会話の流れ:\n")
		// threadPosts はルート投稿から現在の投稿までの順序で並んでいる前提。
		for i, p := range threadPosts {
			role := "ユーザー"
			if p.GetCreatorId() == h.botUserID {
				role = "ほめるん（あなた）"
			}
			text := p.GetText()
			if text == "" {
				continue
			}
			b.WriteString("- ")
			b.WriteString(role)
			b.WriteString("の投稿")
			b.WriteString(" (")
			b.WriteString(fmt.Sprintf("%d", i+1))
			b.WriteString("): ")
			b.WriteString(text)
			b.WriteString("\n")
		}
		historyText = b.String()
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

	compliment, err := h.gemini.GenerateCompliment(ctx, post.GetText(), imageURL, userName, isReplyToBot, historyText)
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

	// スタンプは「アプリがメンションされている投稿」に対してのみ付与する。
	// リプライのみ（メンションなし）の場合にスタンプを付けようとすると、
	// API 側で "can't stamp because application is not mentioned" エラーになるため。
	if isMention {
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
	}
	return nil
}

// collectThreadIfReplyToBot は、指定した投稿がボットの投稿を含むスレッドの一部かどうかを判定し、
// その場合はルートのユーザー投稿から現在の投稿までのポスト一覧を返す。
// 戻り値の bool は「スレッド内にボットの投稿が含まれているか」を表す。
func (h *MyHandler) collectThreadIfReplyToBot(ctx context.Context, post *modelv1.Post) (bool, []*modelv1.Post) {
	if post == nil {
		return false, nil
	}

	authCtx, err := h.authenticator.AuthorizedContext(ctx)
	if err != nil {
		log.Printf("failed to attach auth context for GetPosts in collectThreadIfReplyToBot: %v", err)
		return false, nil
	}

	var chain []*modelv1.Post
	current := post

	// 返信チェーンを親方向にたどり、現在の投稿からルート投稿までを収集する。
	for {
		chain = append(chain, current)

		inReplyToID := current.GetInReplyToPostId()
		if inReplyToID == "" {
			break
		}

		resp, err := h.client.GetPosts(authCtx, &application_apiv1.GetPostsRequest{
			PostIdList: []string{inReplyToID},
		})
		if err != nil {
			log.Printf("failed to get parent post in collectThreadIfReplyToBot: %v", err)
			break
		}
		posts := resp.GetPosts()
		if len(posts) == 0 {
			break
		}
		current = posts[0]
	}

	if len(chain) == 0 {
		return false, nil
	}

	// スレッド内にボットの投稿が含まれているかを判定。
	hasBotPost := false
	for _, p := range chain {
		if p.GetCreatorId() == h.botUserID {
			hasBotPost = true
			break
		}
	}
	if !hasBotPost {
		return false, nil
	}

	// ルート（最初の投稿）から現在の投稿までの順序に並べ替える。
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	return true, chain
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
