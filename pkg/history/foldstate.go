package history

// FoldState indexes the records a folded history contains, keyed by the IDs
// events refer to them by, so the fold resolves a target without scanning.
//
// The maps hold the same pointers as the history, so updating a record through
// FoldState updates the history with it.
type FoldState struct {
	toolCalls  map[string]*ToolCall
	arcs       map[string]*Arc
	interrupts map[string]*Interrupt
	asyncTasks map[string]*AsyncTask
	responses  map[string]*AssistantMessage
}

// NewFoldState returns the empty index a history is folded from the first
// event onwards. Use FoldStateFromHistory to resume folding into a history
// that already has records in it.
func NewFoldState() *FoldState {
	return &FoldState{
		toolCalls:  make(map[string]*ToolCall),
		arcs:       make(map[string]*Arc),
		interrupts: make(map[string]*Interrupt),
		asyncTasks: make(map[string]*AsyncTask),
		responses:  make(map[string]*AssistantMessage),
	}
}
