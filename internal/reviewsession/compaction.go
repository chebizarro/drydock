package reviewsession

import (
	"encoding/json"
	"fmt"
	"sort"

	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

type TokenCounter interface {
	Count(string) int
}

// CompactHistory deterministically reconstructs successful conversation history.
// When the raw transcript is over budget, every older completed turn is reduced
// to its exact user text plus the persisted final summary/findings. The newest
// completed turn is never compacted, and no turn is silently dropped.
func CompactHistory(loaded Loaded, counter TokenCounter, budget int) ([]reviewengine.CompletionMessage, error) {
	if counter == nil || budget <= 0 {
		return nil, fmt.Errorf("review session: history counter and positive budget are required")
	}
	complete := make(map[int]Turn)
	var turnNos []int
	for _, turn := range loaded.Turns {
		if turn.Status == TurnComplete {
			complete[turn.TurnNo] = turn
			turnNos = append(turnNos, turn.TurnNo)
		}
	}
	sort.Ints(turnNos)
	if len(turnNos) == 0 {
		return nil, nil
	}
	byTurn := make(map[int][]Message)
	for _, message := range loaded.Messages {
		if _, ok := complete[message.TurnNo]; ok {
			byTurn[message.TurnNo] = append(byTurn[message.TurnNo], message)
		}
	}
	raw := flattenMessages(turnNos, byTurn)
	if historyTokens(raw, counter) <= budget {
		return raw, nil
	}

	newest := turnNos[len(turnNos)-1]
	var compacted []reviewengine.CompletionMessage
	for _, turnNo := range turnNos {
		if turnNo == newest {
			for _, message := range byTurn[turnNo] {
				compacted = append(compacted, message.CompletionMessage())
			}
			continue
		}
		turn := complete[turnNo]
		if turn.RequestText != "" {
			compacted = append(compacted, reviewengine.CompletionMessage{
				Role: reviewengine.MessageRoleUser, Content: turn.RequestText,
			})
		}
		summary, err := compactedTurnResult(turn)
		if err != nil {
			return nil, err
		}
		compacted = append(compacted, reviewengine.CompletionMessage{
			Role: reviewengine.MessageRoleAssistant, Content: summary,
		})
	}
	if historyTokens(compacted, counter) > budget {
		return nil, ErrHistoryTooLarge
	}
	return compacted, nil
}

func flattenMessages(turnNos []int, byTurn map[int][]Message) []reviewengine.CompletionMessage {
	var history []reviewengine.CompletionMessage
	for _, turnNo := range turnNos {
		messages := append([]Message(nil), byTurn[turnNo]...)
		sort.Slice(messages, func(i, j int) bool { return messages[i].Seq < messages[j].Seq })
		for _, message := range messages {
			history = append(history, message.CompletionMessage())
		}
	}
	return history
}

func historyTokens(messages []reviewengine.CompletionMessage, counter TokenCounter) int {
	encoded, err := json.Marshal(messages)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return counter.Count(string(encoded))
}

func compactedTurnResult(turn Turn) (string, error) {
	if len(turn.Result) == 0 || !json.Valid(turn.Result) {
		return "", fmt.Errorf("%w: turn %d has no valid terminal result", ErrInvalidTranscript, turn.TurnNo)
	}
	// turn.Result is always json.Marshal(reviewengine.RunOutput); the summary and
	// findings live at the fixed path .Review.summary / .Review.findings. Decode
	// that shape directly so reconstruction is deterministic. Findings stay as
	// raw bytes to preserve the persisted representation verbatim.
	var out struct {
		Review struct {
			Summary  string          `json:"summary"`
			Findings json.RawMessage `json:"findings"`
		}
	}
	if err := json.Unmarshal(turn.Result, &out); err != nil {
		return "", err
	}
	findings := out.Review.Findings
	if len(findings) == 0 {
		findings = json.RawMessage(`[]`)
	}
	payload := struct {
		TurnNo   int             `json:"turn_no"`
		Summary  string          `json:"summary"`
		Findings json.RawMessage `json:"findings"`
	}{
		TurnNo: turn.TurnNo, Summary: out.Review.Summary, Findings: findings,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return "Prior review result: " + string(encoded), nil
}
