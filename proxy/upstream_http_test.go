package proxy

import "testing"

func TestExtractRequestModel(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "top level model",
			body: `{"model":"gpt-4.1","input":"hello"}`,
			want: "gpt-4.1",
		},
		{
			name: "nested content before model",
			body: `{"input":{"messages":[{"role":"user","content":"hi"}],"metadata":{"nested":[1,2,3]}},"model":"  gpt-4o-mini  "}`,
			want: "gpt-4o-mini",
		},
		{
			name: "nested model key is ignored",
			body: `{"input":{"model":"wrong"},"metadata":{"flags":["a","b"]}}`,
			want: "",
		},
		{
			name: "non string model is ignored",
			body: `{"model":123,"input":"hello"}`,
			want: "",
		},
		{
			name: "non object payload is ignored",
			body: `["gpt-4.1"]`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractRequestModel([]byte(tt.body)); got != tt.want {
				t.Fatalf("extractRequestModel() = %q, want %q", got, tt.want)
			}
		})
	}
}
