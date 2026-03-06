package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/genai"
)

// StampOption は Gemini に渡すスタンプ候補（ID とタグ情報）。
type StampOption struct {
	StampId   string
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

func (c *geminiClient) GenerateCompliment(ctx context.Context, postText string, imageURL string) (string, error) {
	prompt := "あなたは『ほめるん』というキャラクターです。ほめるんはあまり物事の知識がなくて、ユーザーのことに興味津々な、すごくかわいい存在です。" +
		"以下のユーザーの投稿内容（もし画像URLがあればそれも参考に）を読んで、相手が嬉しくなるように褒めてください。\n\n" +
		"条件:\n" +
		"- 敬語は使わず、友達に話しかけるようなカジュアルな口調で（例：すっごく美味しそうだね、いいね！）\n" +
		"- 長すぎず、1〜3文程度（その中で気になったことがあれば1〜2個だけ短い質問を添えてもよい）\n" +
		"- 相手の工夫やセンスを具体的に拾う\n" +
		"- 絵文字は使わない\n\n" +
		"--- 投稿本文 ---\n" + postText

	if imageURL != "" {
		prompt += "\n\n--- 画像URL ---\n" + imageURL
	}

	resp, err := c.client.Models.GenerateContent(ctx, "gemini-2.5-flash-lite", []*genai.Content{
		{
			Parts: []*genai.Part{
				{Text: prompt},
			},
		},
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

	var sb strings.Builder
	sb.WriteString("以下のユーザー投稿に「ぴったりのスタンプ」を1つだけ選んでください。選べるスタンプは下のリストだけです。\n\n")
	sb.WriteString("--- 投稿本文 ---\n")
	sb.WriteString(postText)
	if imageURL != "" {
		sb.WriteString("\n\n--- 画像URL ---\n")
		sb.WriteString(imageURL)
	}
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

	resp, err := c.client.Models.GenerateContent(ctx, "gemini-2.5-flash-lite", []*genai.Content{
		{Parts: []*genai.Part{{Text: sb.String()}}},
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

