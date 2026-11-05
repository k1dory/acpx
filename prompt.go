package acpx

import (
	"net/url"
	"path"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// PromptBlocks is a prompt's content: the text, then a resource_link for each attached file
// (an absolute path in the agent's file system). Every ACP agent takes text and resource links
// (the baseline prompt capabilities): the agent sees the files as files, not as words in the text.
func PromptBlocks(text string, files []string) []acp.ContentBlock {
	blocks := []acp.ContentBlock{acp.TextBlock(text)}
	for _, f := range files {
		if !strings.HasPrefix(f, "/") {
			continue
		}
		u := url.URL{Scheme: "file", Path: path.Clean(f)}
		blocks = append(blocks, acp.ResourceLinkBlock(path.Base(f), u.String()))
	}
	return blocks
}
