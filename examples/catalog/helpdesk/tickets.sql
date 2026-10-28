-- The statements of the helpdesk team. Each -- name: line starts one
-- statement and names its tool. tools.yaml in this folder says which of
-- them become tools, with their parameters in $1 order. adaptive-pg-mcp
-- check prepares every statement on each of its databases.

-- name: tickets_by_status
SELECT status, count(*) AS tickets
FROM public.tickets
WHERE opened_on >= $1
GROUP BY status
ORDER BY status;

-- name: close_stale_tickets
UPDATE public.tickets
SET status = 'closed'
WHERE status = 'pending'
  AND updated_on < current_date - $1::int4
RETURNING id;
