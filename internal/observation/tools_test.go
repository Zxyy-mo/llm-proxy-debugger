package observation

import "testing"

func TestStreamingToolArgumentsAndHistoryResults(t *testing.T) {
	c := New("openai")
	for _, data := range []string{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"lookup","arguments":"{\"id\":"}}]}}]}`, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"9007199254740993}"}}]}}]}`} {
		c.Feed([]byte(data))
	}
	calls := c.Calls()
	if len(calls) != 1 || calls[0].Input != `{"id":9007199254740993}` || calls[0].Duration != nil || calls[0].Status != "requested" {
		t.Fatalf("tool incorrectly reconstructed: %+v", calls)
	}
	results := Results([]byte(`{"messages":[{"role":"tool","tool_call_id":"call-1","content":"found"}]}`))
	if len(results) != 1 || results[0].CallID != "call-1" || results[0].Output != "found" {
		t.Fatal("tool results not recognized")
	}
}

func TestResponsesFinalCallReplacesDeltaArguments(t *testing.T) {
	c := New("responses")
	for _, data := range []string{`{"type":"response.output_item.added","item":{"id":"item-1","call_id":"call-1","type":"function_call","name":"lookup","arguments":""}}`, `{"type":"response.function_call_arguments.delta","item_id":"item-1","delta":"{}"}`, `{"type":"response.output_item.done","item":{"id":"item-1","call_id":"call-1","type":"function_call","name":"lookup","arguments":"{}"}}`} {
		c.Feed([]byte(data))
	}
	calls := c.Calls()
	if len(calls) != 1 || calls[0].Input != "{}" {
		t.Fatalf("arguments duplicated: %+v", calls)
	}
}
