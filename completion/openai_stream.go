package completion

import (
	"encoding/json"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/apihttp"
)

// chatStream assembles a streamed Chat Completions response into the shape a
// non-streaming one has, so one set of terminal rules judges both: a missing
// finish, incomplete tool arguments or an unknown tool fail the same way
// however they arrived. It keeps only what that shape carries.
type chatStream struct {
	content, reasoning, refusal strings.Builder
	sawContent, sawReasoning    bool
	sawRefusal                  bool
	details                     []json.RawMessage
	calls                       []*streamedCall
	finish                      *string
	usage                       json.RawMessage
	failure                     json.RawMessage
	sawChoice                   bool
}

type streamedCall struct {
	id, kind, name string
	arguments      strings.Builder
	extra          json.RawMessage
}

// maxStreamedCalls bounds the calls one response may open; parsing refuses
// more than maxToolProposals anyway.
const maxStreamedCalls = 64

type chatChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          *string           `json:"content"`
			ReasoningContent *string           `json:"reasoning_content"`
			ReasoningDetails []json.RawMessage `json:"reasoning_details"`
			Refusal          *string           `json:"refusal"`
			ToolCalls        []chatCallDelta   `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// chatCallDelta is one fragment of a streamed tool call: the first names it,
// and the rest carry pieces of its arguments.
type chatCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	ExtraContent json.RawMessage `json:"extra_content"`
}

// add folds one event in. A chunk this library cannot read ends the stream as
// malformed; a failure reported mid-stream is kept to be classified.
func (s *chatStream) add(data []byte) error {
	var chunk chatChunk
	if json.Unmarshal(data, &chunk) != nil {
		return apihttp.ResponseFailure("malformed_response")
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		s.failure = chunk.Error
		return nil
	}
	if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
		// Only the terminal chunk should carry it; where every chunk does, the
		// last is the whole response's.
		s.usage = chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			return apihttp.ResponseFailure("unexpected_choice_count")
		}
		s.sawChoice = true
		delta := choice.Delta
		if delta.Content != nil {
			s.content.WriteString(*delta.Content)
			s.sawContent = true
		}
		if delta.ReasoningContent != nil {
			s.reasoning.WriteString(*delta.ReasoningContent)
			s.sawReasoning = true
		}
		if delta.Refusal != nil {
			s.refusal.WriteString(*delta.Refusal)
			s.sawRefusal = true
		}
		s.details = append(s.details, delta.ReasoningDetails...)
		for _, fragment := range delta.ToolCalls {
			if err := s.addCall(fragment); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil {
			if s.finish != nil {
				return apihttp.ResponseFailure("ambiguous_terminal_state")
			}
			s.finish = choice.FinishReason
		}
	}
	return nil
}

// addCall merges one fragment into the call its index names.
func (s *chatStream) addCall(fragment chatCallDelta) error {
	if fragment.Index == nil || *fragment.Index < 0 || *fragment.Index >= maxStreamedCalls {
		return apihttp.ResponseFailure("invalid_tool_call")
	}
	for len(s.calls) <= *fragment.Index {
		s.calls = append(s.calls, &streamedCall{})
	}
	call := s.calls[*fragment.Index]
	call.id += fragment.ID
	call.kind += fragment.Type
	call.name += fragment.Function.Name
	call.arguments.WriteString(fragment.Function.Arguments)
	if len(fragment.ExtraContent) > 0 && string(fragment.ExtraContent) != "null" {
		call.extra = fragment.ExtraContent
	}
	return nil
}

// response is the stream as one Chat Completions response.
func (s *chatStream) response() []byte {
	out := chatCompletionResponse{Error: s.failure, Usage: s.usage}
	if s.sawChoice {
		out.Choices = []chatResponseChoice{{FinishReason: s.finish, Message: s.reply()}}
	}
	data, _ := json.Marshal(out)
	return data
}

// reply is the assembled message. What never arrived stays null, as it would
// in a whole response.
func (s *chatStream) reply() *chatReply {
	reply := &chatReply{Role: "assistant"}
	if s.sawContent {
		reply.Content = rawJSON(s.content.String())
	}
	if s.sawReasoning {
		reply.ReasoningContent = rawJSON(s.reasoning.String())
	}
	if s.sawRefusal {
		refusal := s.refusal.String()
		reply.Refusal = &refusal
	}
	if len(s.details) > 0 {
		reply.ReasoningDetails = rawJSON(s.details)
	}
	for _, call := range s.calls {
		reply.ToolCalls = append(reply.ToolCalls, chatReplyCall{ID: call.id, Type: call.kind, Function: &chatReplyFunction{Name: call.name, Arguments: call.arguments.String()}, ExtraContent: call.extra})
	}
	return reply
}

// rawJSON encodes a value that always has a JSON form.
func rawJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
