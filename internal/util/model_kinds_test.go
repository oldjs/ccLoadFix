package util

import "testing"

func TestIsGPTImageModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-image-1", true},
		{"gpt-image-1-mini", true},
		{"gpt-image-1.5", true},
		{"GPT-Image-1", true},   // 大小写不敏感
		{" gpt-image-1 ", true}, // 去除空格
		{"chatgpt-image-latest", true},
		{"chatgpt-image-preview", true},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-image", false}, // 缺少后面的 "-" 和版本
		{"claude-opus-4-1", false},
		{"gemini-2.5-pro", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsGPTImageModel(c.model); got != c.want {
			t.Errorf("IsGPTImageModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestBodyLooksLikeStreamIncomplete(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "user reported 408 body",
			body: `{"error":{"message":"stream error: stream disconnected before completion: stream closed before response.completed","type":"invalid_request_error"}}`,
			want: true,
		},
		{
			name: "only response.completed mention",
			body: `{"error":{"message":"never saw response.completed"}}`,
			want: true,
		},
		{
			name: "stream closed alone",
			body: `{"error":{"message":"stream closed unexpectedly"}}`,
			want: true,
		},
		{
			name: "uppercase",
			body: `{"error":{"message":"Stream Disconnected Before Completion"}}`,
			want: true,
		},
		{
			name: "normal client error",
			body: `{"error":{"message":"invalid prompt: too long","type":"invalid_request_error"}}`,
			want: false,
		},
		{
			name: "empty",
			body: ``,
			want: false,
		},
	}
	for _, c := range cases {
		if got := BodyLooksLikeStreamIncomplete([]byte(c.body)); got != c.want {
			t.Errorf("%s: BodyLooksLikeStreamIncomplete = %v, want %v", c.name, got, c.want)
		}
	}
}
