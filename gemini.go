package main

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/genai"
)

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
	prompt := "あなたはポジティブでフレンドリーな日本語のアシスタントです。" +
		"以下のユーザーの投稿内容" +
		"（もし画像URLがあればそれも参考に）を、相手が嬉しくなるように褒めてください。\n\n" +
		"条件:\n" +
		"- ですます調で丁寧に\n" +
		"- 長すぎず、1〜3文程度\n" +
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

