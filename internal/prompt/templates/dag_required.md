<planning-required>You created {{count}} tasks but NO dependencies. Before executing any task, you MUST:
1. Think: what is the execution order? Which task must finish before another can start?
2. Call task_add_dep(from, to) to define at least one dependency edge.
3. Call task_dag to review the DAG.
4. Only then start working on ready tasks.
If tasks can run in parallel, that's fine — but you must still call task_dag to confirm.</planning-required>
