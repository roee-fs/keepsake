-- Candidate rankers for bench/rankers.py, installed over a migrated schema. Each takes the
-- tenant explicitly and a query of OR-ed words, as store.Search builds it, and returns
-- paths best first. rankers.py adds okf.rank_shipped, store.Search's own SQL.

-- 1. What 0.2.0 shipped.
CREATE FUNCTION okf.rank_cd(tenant uuid, q text, lim int) RETURNS TABLE (path text, score float8)
LANGUAGE sql STABLE AS $$
  SELECT c.path, ts_rank_cd(c.search, to_tsquery('english', q))::float8
  FROM okf.concept c
  WHERE c.tenant_id = tenant AND c.search @@ to_tsquery('english', q)
  ORDER BY 2 DESC, 1 LIMIT lim
$$;

-- 2. ts_rank, divided by 1 + log(document length).
CREATE FUNCTION okf.rank_ts1(tenant uuid, q text, lim int) RETURNS TABLE (path text, score float8)
LANGUAGE sql STABLE AS $$
  SELECT c.path, ts_rank(c.search, to_tsquery('english', q), 1)::float8
  FROM okf.concept c
  WHERE c.tenant_id = tenant AND c.search @@ to_tsquery('english', q)
  ORDER BY 2 DESC, 1 LIMIT lim
$$;

-- 3. BM25 (k1=0.9, b=0.4) from each candidate's tsvector. Length is distinct lexemes and
-- avgdl is over the candidates, which BEIR scores the same as exact BM25.
CREATE FUNCTION okf.rank_bm25_scan(tenant uuid, q text, lim int) RETURNS TABLE (path text, score float8)
LANGUAGE sql STABLE AS $$
  WITH terms AS (SELECT tsvector_to_array(to_tsvector('english', q)) AS t),
  docs AS (SELECT count(*)::float8 AS n FROM okf.concept c WHERE c.tenant_id = tenant),
  cand AS (
    SELECT c.path AS p, c.search AS v, length(c.search)::float8 AS dl, avg(length(c.search)) OVER () AS avgdl
    FROM okf.concept c WHERE c.tenant_id = tenant AND c.search @@ to_tsquery('english', q)
  ),
  hits AS (
    SELECT k.p, k.dl, k.avgdl, u.lexeme, coalesce(array_length(u.positions, 1), 1)::float8 AS tf
    FROM cand k, terms, unnest(k.v) u WHERE u.lexeme = ANY (terms.t)
  ),
  df AS (SELECT h.lexeme, count(*)::float8 AS n FROM hits h GROUP BY h.lexeme)
  SELECT h.p, sum(ln(1 + (docs.n - df.n + 0.5) / (df.n + 0.5)) * h.tf * 1.9 / (h.tf + 0.9 * (0.6 + 0.4 * h.dl / h.avgdl)))
  FROM hits h JOIN df ON df.lexeme = h.lexeme, docs
  GROUP BY h.p ORDER BY 2 DESC, 1 LIMIT lim
$$;

