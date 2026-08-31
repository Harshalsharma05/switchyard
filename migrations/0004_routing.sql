-- 0004_routing: what Step 8.2's cost-aware routing decided, per request.
--
-- Both columns are NULL for a request whose caller named a model outright:
-- routing never ran, which is distinct from "ran and chose nothing" — the same
-- not-applicable convention cache_hit and quality_score already use.
--
-- routing_reason holds the classifier's rationale verbatim ("tokens=41
-- reasoning=1 score=1.05"). It is stored rather than recomputed because the
-- prompt is not in this table and never will be: without it, Request Logs
-- could show that a decision happened but never explain it. The string is
-- derived feature counts, not content — it reveals nothing the existing
-- input_tokens column does not.

ALTER TABLE requests ADD COLUMN IF NOT EXISTS routing_tier   text;
ALTER TABLE requests ADD COLUMN IF NOT EXISTS routing_reason text;
