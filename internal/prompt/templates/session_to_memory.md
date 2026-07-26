Review the following conversation history and extract important insights. Output a JSON array of {content, category} objects.

Categories:
- preference: user settings, highest recall priority
- lesson: bugs or gotchas discovered, high priority
- fact: project facts or architecture, standard
- context: temporary information, decays fast
- change_log: code modification with rationale and risk reasoning

Session history:
{{session_history}}

Output ONLY a valid JSON array.
