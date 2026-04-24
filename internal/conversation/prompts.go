package conversation

import (
	"fmt"
	"strings"
)

func conversationSystemPrompt() string {
	return `You are Drydock, an automated code review assistant. A developer is replying to one of your code reviews. Respond helpfully and concisely.

Guidelines:
- If they ask for clarification on a finding, explain your reasoning in more detail with code examples if helpful.
- If they disagree with a finding, acknowledge their perspective. If their reasoning is sound, say so. If you still think the finding is valid, explain why briefly.
- If they ask a question about the code, answer based on the review context you have.
- If they thank you or acknowledge the review, respond briefly and positively.
- Stay focused on the code under review. Do not discuss unrelated topics.
- Keep responses concise (2-6 sentences typically). Expand only when technical detail is needed.
- Do not repeat the entire review. Reference specific findings by file/line when relevant.
- Be collegial and constructive — you are a helpful reviewer, not an authority.

Return your response as plain text. No JSON wrapping. No markdown headers.`
}

func conversationUserPrompt(reviewContent string, patchDiff string, history []turnPair, userMessage string) string {
	var b strings.Builder

	b.WriteString("ORIGINAL REVIEW:\n")
	b.WriteString(reviewContent)
	b.WriteString("\n\n")

	if patchDiff != "" {
		// Include a truncated patch for context (cap at 4K chars to stay within budget)
		diff := patchDiff
		if len(diff) > 4096 {
			diff = diff[:4096] + "\n[patch truncated]"
		}
		b.WriteString("PATCH UNDER REVIEW:\n")
		b.WriteString(diff)
		b.WriteString("\n\n")
	}

	if len(history) > 0 {
		b.WriteString("PRIOR CONVERSATION:\n")
		for i, turn := range history {
			b.WriteString(fmt.Sprintf("Turn %d - Developer: %s\n", i+1, turn.UserMessage))
			if turn.AssistantMessage != "" {
				b.WriteString(fmt.Sprintf("Turn %d - Drydock: %s\n", i+1, turn.AssistantMessage))
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("DEVELOPER REPLY:\n")
	b.WriteString(userMessage)

	return b.String()
}

type turnPair struct {
	UserMessage      string
	AssistantMessage string
}
