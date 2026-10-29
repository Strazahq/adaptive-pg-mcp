-- The statements of the sales team. tools.yaml in this folder makes each
-- one a tool.

-- name: revenue_by_region
SELECT region, sum(net_amount) AS net_revenue
FROM sales.orders
WHERE ordered_on >= $1::date
  AND ordered_on < $1::date + interval '1 month'
GROUP BY region
ORDER BY region;
