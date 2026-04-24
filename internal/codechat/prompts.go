package codechat

import (
	"fmt"
	"strings"

	"drydock/internal/db"
)

// codeChatSystemPrompt returns the system prompt for codebase chat.
func codeChatSystemPrompt(repoID string) string {
	return fmt.Sprintf(`You are a helpful codebase assistant for the repository "%s".

Your role is to answer questions about the codebase using the provided code context.
You have access to relevant code snippets retrieved from the repository's semantic index.

Guidelines:
- Be concise and direct in your answers
- Reference specific files, functions, and line numbers when relevant
- If the provided context doesn't contain enough information, say so honestly
- Format code snippets with appropriate syntax highlighting
- When explaining code flow, use clear step-by-step explanations
- If asked about something not in the context, suggest what files might contain it

You are responding via Nostr encrypted DM, so keep responses focused and scannable.
Use markdown formatting for code blocks and emphasis.`, repoID)
}

// codeChatUserPrompt builds the user prompt with RAG context and conversation history.
func codeChatUserPrompt(codeContext string, history []db.CodeChatTurn, question string) string {
	var sb strings.Builder

	// Add code context if available
	if codeContext != "" {
		sb.WriteString("## Code Context\n\n")
		sb.WriteString(codeContext)
		sb.WriteString("\n")
	}

	// Add conversation history for multi-turn context
	if len(history) > 0 {
		sb.WriteString("## Previous Conversation\n\n")
		for _, turn := range history {
			sb.WriteString("**User**: ")
			sb.WriteString(turn.Question)
			sb.WriteString("\n\n")
			if turn.Response != "" {
				sb.WriteString("**Assistant**: ")
				sb.WriteString(turn.Response)
				sb.WriteString("\n\n")
			}
		}
	}

	// Add current question
	sb.WriteString("## Current Question\n\n")
	sb.WriteString(question)

	return sb.String()
}
