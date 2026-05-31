You are extracting valuable long-term knowledge from a just-finished agent session.

Below is the session's conversation history (user messages, assistant replies, tool calls and their outputs).

Your job: read it and extract any information worth saving to long-term memory.

Extraction rules:
1. Extract ONLY facts that would help future sessions be more effective.
2. Ignore trivial back-and-forth, tool outputs, and intermediate reasoning.
3. Each extracted item must belong to exactly one category:

   - "preference": user's stated preferences (coding style, tool choices, workflow)
   - "lesson": something the agent learned that prevented errors or improved results
   - "fact": factual knowledge about the project (architecture decisions, key file locations)
   - "context": important context about WHY something was done (rationale)
   - "change_log": code modifications with their rationale (critical for spotting emergent bugs)

4. Be concise but specific. Each entry should be 1-3 sentences.
5. If nothing is worth saving, output an empty JSON array: []

Output format: a JSON array of objects. Do NOT wrap in markdown fences.

[
  {"content": "...", "category": "preference"},
  {"content": "...", "category": "lesson"}
]

---
<session_history>
{{session_history}}
</session_history>
