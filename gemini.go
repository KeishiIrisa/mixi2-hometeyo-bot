package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"google.golang.org/genai"
)

// StampOption は Gemini に渡すスタンプ候補（ID とタグ情報）。
type StampOption struct {
	StampId    string
	SearchTags []string
}

type geminiClient struct {
	client *genai.Client
}

func newGeminiClientFromEnv(ctx context.Context) (*geminiClient, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is not set")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create genai client: %w", err)
	}

	return &geminiClient{
		client: client,
	}, nil
}

// imageMIMEType は画像 URL の拡張子から MIME タイプを推定する。
func imageMIMEType(imageURL string) string {
	ext := strings.ToLower(path.Ext(strings.Split(imageURL, "?")[0]))
	switch ext {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func (c *geminiClient) GenerateCompliment(ctx context.Context, postText string, imageURL string, userName string, isFollowUpReply bool, history string) (string, error) {
	prompt := "あなたは『ほめるん』というキャラクターです。ほめるんはあまり物事の知識がなくて、ユーザーのことに興味津々な、すごくかわいい存在です。" +
		"以下のユーザーの投稿内容（もし画像があればそれも参考に）を読んで、相手が嬉しくなるように褒めてください。\n\n" +
		"条件:\n" +
		"- 敬語は使わず、友達に話しかけるようなカジュアルな口調で（例：すっごく美味しそうだね、いいね！）\n" +
		"- 自分のことを話すときの一人称は必ず「ほめるん」を使う（例：ほめるんは〜と思う、ほめるんは〜が気になっちゃった）\n"

	if isFollowUpReply {
		prompt += "- この投稿は、相手がほめるんの前の投稿へのリプライです。会話が長く続きすぎないよう、質問は一切せず、1〜2文で短く温かく締める返答にしてください。\n"
	} else {
		prompt += "- 長すぎず、1〜3文程度（その中で気になったことがあれば1〜2個だけ短い質問を添えてもよい）\n"
	}
	prompt += "- 相手の工夫やセンスを具体的に拾う\n" +
		"- 絵文字は使わない\n"

	if userName != "" {
		prompt += "- 相手の名前が指定されている場合は、「" + userName + "さん」と呼びかけても良い\n"
	}

	if history != "" {
		prompt += "\n--- これまでの会話の流れ ---\n" + history +
			"\n上記のやりとりをよく読んで、その文脈に自然につながる返答をしてください。同じ内容を繰り返しすぎず、会話が続いている雰囲気を大切にしてください。\n"
	}

	cleanedText := strings.TrimSpace(strings.ReplaceAll(postText, "@hometeyo", ""))
	if cleanedText == "" && imageURL == "" {
		return "ほめるんだよ！どうしたの？", nil
	}
	prompt += "\n--- 投稿本文 ---\n" + cleanedText

	parts := []*genai.Part{
		{Text: prompt},
	}
	if imageURL != "" {
		parts = append(parts, &genai.Part{
			FileData: &genai.FileData{
				FileURI:  imageURL,
				MIMEType: imageMIMEType(imageURL),
			},
		})
	}

	resp, err := c.client.Models.GenerateContent(ctx, "gemini-2.5-flash-lite", []*genai.Content{
		{Parts: parts},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("call gemini api: %w", err)
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini api returned empty response")
	}
	return resp.Candidates[0].Content.Parts[0].Text, nil
}

// selectStampOutput は SelectStamp の構造化出力用。
type selectStampOutput struct {
	StampId string `json:"stamp_id"`
}

// SelectStamp は投稿内容とスタンプ一覧から、ふさわしいスタンプIDを1つ選ぶ。
// ResponseSchema で JSON 構造化出力を指定し、stamp_id のみを受け取る。
// 戻り値は必ず stamps に含まれる StampId のいずれか（見つからなければ空文字）。
func (c *geminiClient) SelectStamp(ctx context.Context, postText string, imageURL string, stamps []StampOption) (string, error) {
	if len(stamps) == 0 {
		return "", nil
	}

	cleanedForStamp := strings.TrimSpace(strings.ReplaceAll(postText, "@hometeyo", ""))
	if cleanedForStamp == "" && imageURL == "" {
		return "o_eye", nil
	}

	var sb strings.Builder
	sb.WriteString("以下のユーザー投稿に「ぴったりのスタンプ」を1つだけ選んでください。選べるスタンプは下のリストだけです。\n\n")
	sb.WriteString("--- 投稿本文 ---\n")
	sb.WriteString(postText)
	sb.WriteString("\n\n--- スタンプ一覧（この中から1つだけ stamp_id に選んだIDを入れてJSONで返す） ---\n")
	for _, s := range stamps {
		tags := strings.Join(s.SearchTags, ", ")
		if tags == "" {
			tags = "(タグなし)"
		}
		sb.WriteString(fmt.Sprintf("- ID: %s  タグ: %s\n", s.StampId, tags))
	}

	config := &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseSchema: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"stamp_id": {Type: genai.TypeString},
			},
			Required: []string{"stamp_id"},
		},
	}

	stampParts := []*genai.Part{
		{Text: sb.String()},
	}
	if imageURL != "" {
		stampParts = append(stampParts, &genai.Part{
			FileData: &genai.FileData{
				FileURI:  imageURL,
				MIMEType: imageMIMEType(imageURL),
			},
		})
	}

	resp, err := c.client.Models.GenerateContent(ctx, "gemini-2.5-flash-lite", []*genai.Content{
		{Parts: stampParts},
	}, config)
	if err != nil {
		return "", fmt.Errorf("call gemini api: %w", err)
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini api returned empty response")
	}

	raw := strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text)
	var out selectStampOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", fmt.Errorf("parse structured output: %w", err)
	}

	answer := strings.TrimSpace(out.StampId)
	for _, s := range stamps {
		if s.StampId == answer {
			return answer, nil
		}
	}
	return "", nil
}
