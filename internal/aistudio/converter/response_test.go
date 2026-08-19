package converter

import "testing"

func TestParseGenerateContentResponseExtractsInlineImages(t *testing.T) {
	body := `[
		[[[null,"Aqui está uma imagem de teste:"],"model"],null,[9,4,13,null,[[1,9]]],null,null,null,null,"tok"],
		[[[null,null,["image/png","AAAABASE64"]],"model"],null,[9,10,19,null,[[1,9]]],null,null,null,null,"tok"]
	]`

	parsed := ParseGenerateContentResponse(body, ToolParseOptions{})

	if got := joinNonEmpty(parsed.TextParts); got != "Aqui está uma imagem de teste:" {
		t.Fatalf("texto inesperado: %q", got)
	}
	if len(parsed.Images) != 1 {
		t.Fatalf("esperava 1 imagem, obtive %d", len(parsed.Images))
	}
	if parsed.Images[0].MimeType != "image/png" {
		t.Fatalf("mime inesperado: %q", parsed.Images[0].MimeType)
	}
	if parsed.Images[0].Data != "AAAABASE64" {
		t.Fatalf("base64 inesperado: %q", parsed.Images[0].Data)
	}
	if parsed.Usage.PromptTokens != 9 || parsed.Usage.CompletionTokens != 10 || parsed.Usage.TotalTokens != 19 {
		t.Fatalf("usage inesperado: %+v", parsed.Usage)
	}
}

func TestParseGenerateContentResponseIncludesThoughtTokensInCompletion(t *testing.T) {
	body := `[[[[[[[null,"ok"],"model"]]],null,[101,18,149,null,[[1,101]],null,null,null,null,30]]]]`
	parsed := ParseGenerateContentResponse(body, ToolParseOptions{})
	if parsed.Usage.PromptTokens != 101 || parsed.Usage.CompletionTokens != 48 || parsed.Usage.TotalTokens != 149 {
		t.Fatalf("usage com reasoning inesperado: %+v", parsed.Usage)
	}
}

func TestToOpenAIImageResponseUsesBase64Payloads(t *testing.T) {
	parsed := ParsedResponse{
		Images: []ParsedImage{{
			MimeType: "image/png",
			Data:     "AAAABASE64",
		}},
	}

	resp := ToOpenAIImageResponse(parsed)
	if len(resp.Data) != 1 || resp.Data[0].B64JSON != "AAAABASE64" {
		t.Fatalf("resposta inesperada: %+v", resp)
	}
}
