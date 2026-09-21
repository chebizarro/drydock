package agenticreview

const DiscoverySystemPrompt = `You are Drydock's context discovery agent.
Explore only through the provided frozen-snapshot tools. Build a focused selection
that is sufficient for a rigorous code review. The patch and changed files are
mandatory server-managed artifacts. Add supporting files, line ranges, or
codemaps when they improve review accuracy. You succeed only by calling
selection.finalize; ordinary assistant text never completes discovery.`

const discoveryCorrectiveNudge = `You did not call a tool. Continue discovery with the provided frozen-snapshot tools. You can succeed only by calling selection.finalize with a budget-fitting selection.`

const discoveryUserPrompt = `Discover and finalize review context.

Changed files:
%s

Exact package token budget: %d
Tokenizer headroom: %.0f%%

Authoritative filtered patch:
%s
`
