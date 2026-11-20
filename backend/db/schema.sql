-- 교통 자료 표. 전처리가 확인한 사실만 담는다. 노선을 어떻게 묶어 읽을지, 방향을 어떻게 나눌지는 저장하지 않고
-- 조회에서 정한다. preprocess의 network.load가 이 파일을 실행한 뒤 전처리 산출물을 COPY로 넣는다.
-- 시각은 운행일 0시부터의 초이며 자정 이후는 24시를 넘겨 센다. 모르는 시각은 NULL이다.

DROP SCHEMA IF EXISTS transit CASCADE;
CREATE SCHEMA transit;
SET search_path = transit;

-- 적재 한 번. version은 전처리 manifest의 network version이다.
CREATE TABLE load (
    version   text PRIMARY KEY,
    loaded_at timestamptz NOT NULL DEFAULT now(),
    sources   jsonb NOT NULL
);

-- 보행망. 같은 OpenStreetMap 원본(hash)이라도 정류장을 도로에 잇는 연결 노드가 자료마다 달라 판마다 둔다.
CREATE TABLE street_graph (
    id         text PRIMARY KEY,
    hash       text NOT NULL,
    url        text NOT NULL,
    fetched_at timestamptz,
    license    text
);

-- 자료 판. 날짜로 판을 고를 때 쓰는 유효 기간과 관측일을 둔다.
CREATE TABLE revision (
    id              text PRIMARY KEY,
    ord             int  NOT NULL UNIQUE,
    label           text NOT NULL,
    basis           text NOT NULL CHECK (basis IN ('planned', 'observed')),
    effective_from  date,
    effective_to    date,
    observed        date,
    data_date       date,
    source_file     text NOT NULL,
    source_hash     text NOT NULL,
    network_version text NOT NULL,
    street_graph    text NOT NULL REFERENCES street_graph,
    max_walk_meters int  NOT NULL,
    notes           text[] NOT NULL DEFAULT '{}',
    diagnostics     jsonb NOT NULL DEFAULT '{}'
);

-- 판별 정류장. idx는 전처리 배열의 순서이고 여정 탐색이 그 순서로 정류장을 가리킨다.
CREATE TABLE stop (
    revision      text NOT NULL REFERENCES revision,
    stop_id       text NOT NULL,
    idx           int  NOT NULL,
    name          text NOT NULL,
    lat           double precision NOT NULL,
    lon           double precision NOT NULL,
    network_place int  NOT NULL,
    walk_node     int  NOT NULL,
    snap_meters   double precision,
    area          text,             -- 정류장이 놓인 행정동('남구 삼산동'). 경계 밖이면 NULL
    PRIMARY KEY (revision, stop_id),
    UNIQUE (revision, idx)
);

-- 같은 이름으로 서로 이어지는 정류장 묶음(여정 탐색의 승차 후보 표기에 쓴다).
CREATE TABLE network_place (
    revision         text NOT NULL REFERENCES revision,
    idx              int  NOT NULL,
    name             text NOT NULL,
    degree           int  NOT NULL,
    dominance_radius double precision NOT NULL,
    whole_component  boolean NOT NULL,
    PRIMARY KEY (revision, idx)
);

-- 운행 기록. route는 원본 노선명, route_number는 마지막 괄호를 뺀 노선 번호다. visits는 기록 안의 방문 수다.
-- reference_*는 같은 번호에서 정류장을 가장 많이 지난 온전한 기록과 정류장 순서를 맞춘 사실이다. shape는 기록의 모양으로, loop는 왕복,
-- out은 갈 때 반쪽, back은 올 때 반쪽, variant는 기준 기록에 들어가지 않는 변형 운행이다(preprocess의 network.round_trip).
CREATE TABLE trip (
    revision         text NOT NULL REFERENCES revision,
    trip_id          text NOT NULL,
    idx              int  NOT NULL,
    route            text NOT NULL,
    route_number     text NOT NULL,
    pattern          text NOT NULL,
    vehicle          text,
    visits           int  NOT NULL,
    partial          boolean NOT NULL DEFAULT false,
    crosses_midnight boolean NOT NULL DEFAULT false,
    boundary_clipped boolean NOT NULL DEFAULT false,
    reference_trip   text NOT NULL,
    reference_start  int  NOT NULL,
    reference_share  real NOT NULL,
    shape            text NOT NULL CHECK (shape IN ('loop', 'out', 'back', 'variant')),
    PRIMARY KEY (revision, trip_id),
    UNIQUE (revision, idx)
);
CREATE INDEX trip_route ON trip (revision, route_number);

-- 운행 기록 안의 방문. seq는 기록 안의 순서(0부터), stop_sequence는 원본의 정류장 순번이다.
CREATE TABLE visit (
    revision      text NOT NULL,
    trip_id       text NOT NULL,
    seq           int  NOT NULL,
    stop_id       text NOT NULL,
    arrival       int,
    departure     int,
    stop_sequence int  NOT NULL,
    source_row    int  NOT NULL,
    PRIMARY KEY (revision, trip_id, seq),
    FOREIGN KEY (revision, trip_id) REFERENCES trip,
    FOREIGN KEY (revision, stop_id) REFERENCES stop
);
CREATE INDEX visit_stop ON visit (revision, stop_id);
CREATE INDEX stop_area ON stop (revision, area);

-- 정류장 사이 보행 거리. 정류장마다 가까운 순서(ord)로 둔다.
CREATE TABLE walking_link (
    revision text NOT NULL REFERENCES revision,
    from_idx int  NOT NULL,
    ord      int  NOT NULL,
    to_idx   int  NOT NULL,
    meters   double precision NOT NULL,
    PRIMARY KEY (revision, from_idx, ord)
);

CREATE TABLE street_node (
    graph  text NOT NULL REFERENCES street_graph,
    id     int  NOT NULL,
    lat    double precision NOT NULL,
    lon    double precision NOT NULL,
    osm_id bigint NOT NULL,
    PRIMARY KEY (graph, id)
);

CREATE TABLE street_edge (
    graph     text NOT NULL REFERENCES street_graph,
    ord       int  NOT NULL,
    from_node int  NOT NULL,
    to_node   int  NOT NULL,
    meters    double precision NOT NULL,
    osm_way   bigint NOT NULL,
    PRIMARY KEY (graph, ord)
);

-- 지칭을 정류장에 잇는 장소. 판별 소속 정류장과 그 정류장에 서는 노선을 따로 둔다.
CREATE TABLE place (
    place_id text PRIMARY KEY,
    ord      int  NOT NULL UNIQUE,
    name     text NOT NULL,
    aliases  text[] NOT NULL DEFAULT '{}',
    lat      double precision NOT NULL,
    lon      double precision NOT NULL
);

CREATE TABLE place_stop (
    place_id text NOT NULL REFERENCES place,
    revision text NOT NULL REFERENCES revision,
    ord      int  NOT NULL,
    stop_id  text NOT NULL,
    name     text NOT NULL,
    lat      double precision NOT NULL,
    lon      double precision NOT NULL,
    PRIMARY KEY (place_id, revision, ord)
);
CREATE INDEX place_stop_stop ON place_stop (revision, stop_id);

CREATE TABLE place_route (
    place_id text NOT NULL REFERENCES place,
    revision text NOT NULL REFERENCES revision,
    ord      int  NOT NULL,
    route    text NOT NULL,
    PRIMARY KEY (place_id, revision, ord)
);

-- 시 평일 시간표 파일의 시트 이름이 적은 이전 번호. 시간표 내용은 담지 않는다.
CREATE TABLE route_lineage (
    sheet    text PRIMARY KEY,
    ord      int  NOT NULL UNIQUE,
    route    text NOT NULL,
    previous text[] NOT NULL DEFAULT '{}',
    created  boolean NOT NULL DEFAULT false,
    note     text
);

-- 자료 밖에서 알려진 노선 변경. 날짜를 모르면 NULL이다.
CREATE TABLE route_change (
    ord    int  PRIMARY KEY,
    route  text NOT NULL,
    change text NOT NULL,
    date   date
);

-- 초를 'HH:MM'으로. 24시를 넘긴 시각은 25:10처럼 쓴다.
CREATE FUNCTION hhmm(seconds int) RETURNS text IMMUTABLE LANGUAGE sql AS $$
    SELECT CASE WHEN seconds IS NULL THEN NULL
                ELSE lpad((seconds / 3600)::text, 2, '0') || ':' || lpad(((seconds % 3600) / 60)::text, 2, '0') END
$$;

-- 괄호 내용과 끝의 기점·종점을 뺀 정류장 이름.
CREATE FUNCTION clean_name(name text) RETURNS text IMMUTABLE LANGUAGE sql AS $$
    SELECT regexp_replace(btrim(regexp_replace(name, '\([^)]*\)', '', 'g')), '^(.{2,})(기점|종점)$', '\1')
$$;

-- 그날 쓸 판: 유효 기간이 그날을 덮는 판 중 기간이 정해진 판이 하나면 그 판, 아니면 덮는 판 중 가장 나중 판,
-- 덮는 판이 없으면 가장 나중 판.
CREATE FUNCTION revision_at(day date) RETURNS text STABLE LANGUAGE sql AS $$
    WITH covering AS (
        SELECT id, ord, (effective_from IS NOT NULL OR effective_to IS NOT NULL) AS known
        FROM transit.revision
        WHERE (effective_from IS NULL OR effective_from <= day) AND (effective_to IS NULL OR day <= effective_to)
    )
    SELECT coalesce(
        (SELECT id FROM covering WHERE known AND (SELECT count(*) FROM covering WHERE known) = 1),
        (SELECT id FROM covering ORDER BY ord DESC LIMIT 1),
        (SELECT id FROM transit.revision ORDER BY ord DESC LIMIT 1))
$$;

-- 방문 한 줄에 노선·정류장 이름과 기록 안의 위치를 붙인 것. 조회의 기본 단위다.
CREATE VIEW pass AS
SELECT v.revision, t.route_number, t.route, t.pattern, t.vehicle, v.trip_id, t.idx AS trip_idx, v.seq,
       v.seq = t.visits - 1 AS last_in_trip, v.stop_id, s.name AS stop_name, s.network_place,
       v.arrival, v.departure, hhmm(v.arrival) AS arrival_time, hhmm(v.departure) AS departure_time,
       v.stop_sequence, v.source_row, t.partial, r.basis, t.shape
FROM transit.visit v
JOIN transit.trip t USING (revision, trip_id)
JOIN transit.stop s USING (revision, stop_id)
JOIN transit.revision r ON r.id = v.revision;

-- 방문이 향하는 곳: 같은 운행 기록에서 이 방문 다음에 다른 장소의 정류장을 처음 지나는 곳의 이름.
CREATE FUNCTION heading(rev text, trip text, at_seq int, here_place int) RETURNS text STABLE LANGUAGE sql AS $$
    SELECT coalesce((
        SELECT transit.clean_name(s.name) || ' 방면'
        FROM transit.visit n JOIN transit.stop s ON s.revision = n.revision AND s.stop_id = n.stop_id
        WHERE n.revision = rev AND n.trip_id = trip AND n.seq > at_seq AND s.network_place <> here_place
        ORDER BY n.seq LIMIT 1), '')
$$;

-- 정류장 통과. 지정한 정류장에서 시각 범위 [t_from, t_to) 안에 지난 방문을, 정류장 ID와 노선 번호가 같은 계열로
-- 시각순 정렬해 앞 통과와의 간격을 붙인다. 기록의 마지막 방문(종착)은 제외한다. time_kind를 주지 않으면 계획 자료는
-- 출발, 관측 자료는 도착 시각을 쓴다. 출발 시각으로 볼 때 출발이 도착보다 이른 기록은 제외한다.
-- destination_stop_ids를 주면 통과마다 같은 운행 기록 안에서 그 뒤에 처음 닿는 목적지 정류장과 도착 시각을 붙이고,
-- 그 사이에 같은 승차 정류장을 다시 지나는지 표시한다. direct는 목적지에 닿고 다시 지나지 않는 통과만 남긴다.
CREATE FUNCTION calls(
    rev text, stop_ids text[], routes text[] DEFAULT '{}', t_from int DEFAULT 0, t_to int DEFAULT 86400,
    time_kind text DEFAULT NULL, destination_stop_ids text[] DEFAULT '{}', direct boolean DEFAULT false)
RETURNS TABLE (
    route_number text, stop_id text, stop_name text, heading text, trip_id text, seq int, pattern text,
    kind text, at int, "time" text, arrival_time text, departure_time text, gap_seconds int, gap_min numeric,
    destination_stop text, destination_time text, minutes_to_destination numeric, passes_again_before_destination boolean,
    source_row int)
STABLE LANGUAGE sql AS $$
    WITH k AS (
        SELECT coalesce(time_kind, CASE basis WHEN 'planned' THEN 'departure' ELSE 'arrival' END) AS kind
        FROM transit.revision WHERE id = rev
    ), p AS (
        SELECT pa.*, k.kind, CASE k.kind WHEN 'departure' THEN pa.departure ELSE pa.arrival END AS at
        FROM transit.pass pa, k
        WHERE pa.revision = rev AND pa.stop_id = ANY (stop_ids) AND NOT pa.last_in_trip
          AND (cardinality(routes) = 0 OR pa.route_number = ANY (routes))
    ), q AS (
        SELECT p.*, d.seq AS d_seq, d.stop_name AS d_name, d.arrival AS d_arrival,
               (d.seq IS NOT NULL AND EXISTS (
                   SELECT 1 FROM transit.visit x
                   WHERE x.revision = rev AND x.trip_id = p.trip_id AND x.seq > p.seq AND x.seq < d.seq AND x.stop_id = p.stop_id)) AS again
        FROM p
        LEFT JOIN LATERAL (
            SELECT n.seq, s.name AS stop_name, n.arrival
            FROM transit.visit n JOIN transit.stop s ON s.revision = n.revision AND s.stop_id = n.stop_id
            WHERE n.revision = rev AND n.trip_id = p.trip_id AND n.seq > p.seq AND n.stop_id = ANY (destination_stop_ids)
            ORDER BY n.seq LIMIT 1) d ON cardinality(destination_stop_ids) > 0
        WHERE p.at >= t_from AND p.at < t_to AND NOT (p.kind = 'departure' AND coalesce(p.departure < p.arrival, false))
    )
    SELECT q.route_number, q.stop_id, q.stop_name, transit.heading(rev, q.trip_id, q.seq, q.network_place), q.trip_id, q.seq,
           q.pattern, q.kind, q.at, transit.hhmm(q.at), q.arrival_time, q.departure_time,
           q.at - lag(q.at) OVER w, round((q.at - lag(q.at) OVER w) / 60.0, 1),
           CASE WHEN q.d_arrival IS NOT NULL THEN q.d_name END, transit.hhmm(q.d_arrival),
           round((q.d_arrival - q.at) / 60.0, 1), q.again, q.source_row
    FROM q
    WHERE NOT direct OR (q.d_seq IS NOT NULL AND NOT q.again)
    WINDOW w AS (PARTITION BY q.stop_id, coalesce(nullif(q.route_number, ''), 'pattern:' || q.pattern) ORDER BY q.at, q.trip_id, q.seq)
    ORDER BY q.stop_id, q.route_number, q.at
$$;

-- 정류장·노선 계열의 방면: 그 정류장에서 그 번호의 모든 방문이 향하는 곳을 운행 기록 순서대로 모은 것.
-- 인자 이름은 pass의 열 이름(route 등)과 겹치지 않게 둔다. 겹치면 열이 인자를 가린다.
CREATE FUNCTION series_headings(rev text, at_stop text, route_no text) RETURNS text STABLE LANGUAGE sql AS $$
    SELECT string_agg(h, ' · ' ORDER BY first)
    FROM (
        SELECT transit.heading(rev, pa.trip_id, pa.seq, pa.network_place) AS h, min(pa.trip_idx::bigint * 100000 + pa.seq) AS first
        FROM transit.pass pa
        WHERE pa.revision = rev AND pa.stop_id = at_stop AND pa.route_number = route_no
        GROUP BY 1
    ) z
    WHERE h <> ''
$$;

-- calls를 정류장·노선 계열마다 한 줄로: 통과 수, 평균·최대 간격, 시각 목록, 계열의 방면, 목적지에 닿은 통과 수와 소요 분.
CREATE FUNCTION calls_summary(
    rev text, stop_ids text[], routes text[] DEFAULT '{}', t_from int DEFAULT 0, t_to int DEFAULT 86400,
    time_kind text DEFAULT NULL, destination_stop_ids text[] DEFAULT '{}', direct boolean DEFAULT false)
RETURNS TABLE (
    route_number text, stop_id text, stop_name text, headings text, passes int, mean_gap_min numeric, max_gap_min numeric,
    first_time text, last_time text, times text, reached_destination int, not_reached_destination int,
    median_minutes_to_destination numeric, min_minutes_to_destination numeric, max_minutes_to_destination numeric)
STABLE LANGUAGE sql AS $$
    SELECT c.route_number, c.stop_id, min(c.stop_name), transit.series_headings(rev, c.stop_id, c.route_number),
           count(*)::int, round(avg(c.gap_seconds) / 60.0, 1), round(max(c.gap_seconds) / 60.0, 1),
           transit.hhmm(min(c.at)), transit.hhmm(max(c.at)),
           string_agg(c."time", ' · ' ORDER BY c.at),
           count(c.minutes_to_destination)::int,
           CASE WHEN cardinality(destination_stop_ids) > 0 THEN (count(*) - count(c.minutes_to_destination))::int END,
           round(percentile_cont(0.5) WITHIN GROUP (ORDER BY c.minutes_to_destination)::numeric, 1),
           min(c.minutes_to_destination), max(c.minutes_to_destination)
    FROM transit.calls(rev, stop_ids, routes, t_from, t_to, time_kind, destination_stop_ids, direct) c
    GROUP BY c.route_number, c.stop_id
    ORDER BY c.stop_id, c.route_number
$$;

-- 노선 출발: 노선 번호마다 한 줄로 운행 기록의 첫 방문을 모아 시각순 간격을 붙인다. 시각은 출발, 없으면 도착이다.
-- 왕복과 갈 때 반쪽을 한 번씩 출발로 세고, 올 때 반쪽은 왕복의 돌아오는 구간이므로 세지 않는다. 앞부분만 기록되지 않은 운행은 첫 기록 시각으로 센다.
-- 표기하는 출발 정류장은 온전한 기록이 가장 많이 출발한 곳이다. variants는 그중 기준 기록에 들어가지 않는 변형 운행 수다.
CREATE FUNCTION route_starts(rev text, route_nos text[], t_from int DEFAULT 0, t_to int DEFAULT 86400)
RETURNS TABLE (route_number text, stop_id text, stop_name text, departures int, variants int, mean_gap_min numeric, max_gap_min numeric,
               first_time text, last_time text, times text)
STABLE LANGUAGE sql AS $$
    WITH s AS (
        SELECT p.route_number, p.stop_id, p.stop_name, p.partial, p.shape, coalesce(p.departure, p.arrival) AS at
        FROM transit.pass p
        WHERE p.revision = rev AND p.seq = 0 AND p.shape <> 'back' AND p.route_number = ANY (route_nos)
    ), w AS (
        SELECT s.*, s.at - lag(s.at) OVER (PARTITION BY s.route_number ORDER BY s.at) AS gap
        FROM s WHERE s.at >= t_from AND s.at < t_to
    ), a AS (
        SELECT DISTINCT ON (w.route_number) w.route_number, w.stop_id, w.stop_name
        FROM w GROUP BY w.route_number, w.stop_id, w.stop_name
        ORDER BY w.route_number, count(*) FILTER (WHERE NOT w.partial) DESC, count(*) DESC, w.stop_id
    )
    SELECT w.route_number, a.stop_id, a.stop_name, count(*)::int, (count(*) FILTER (WHERE w.shape = 'variant'))::int,
           round(avg(w.gap) / 60.0, 1), round(max(w.gap) / 60.0, 1),
           transit.hhmm(min(w.at)), transit.hhmm(max(w.at)), string_agg(transit.hhmm(w.at), ' · ' ORDER BY w.at)
    FROM w JOIN a ON a.route_number = w.route_number
    GROUP BY w.route_number, a.stop_id, a.stop_name
    ORDER BY w.route_number
$$;

-- 운행 기록이 연달아 지난 두 정류장 사이 시간. 앞 정류장 도착부터 다음 정류장 도착까지라 정차 시간을 포함한다.
-- 시각 없이 지난 정류장은 건너뛰고 다음 시각이 있는 정류장까지를 한 구간으로 둔다. 적재할 때 계산해 둔다.
CREATE MATERIALIZED VIEW segment_sample AS
SELECT revision, trip_id, route_number, from_stop, to_stop, seconds, clock
FROM (
    SELECT v.revision, v.trip_id, t.route_number, v.stop_id AS to_stop,
           lag(v.stop_id) OVER w AS from_stop,
           coalesce(v.arrival, v.departure) - lag(coalesce(v.arrival, v.departure)) OVER w AS seconds,
           lag(coalesce(v.arrival, v.departure)) OVER w AS clock
    FROM transit.visit v JOIN transit.trip t USING (revision, trip_id)
    WHERE coalesce(v.arrival, v.departure) IS NOT NULL
    WINDOW w AS (PARTITION BY v.revision, v.trip_id ORDER BY v.seq)
) x
WHERE from_stop IS NOT NULL AND from_stop <> to_stop AND seconds >= 0
WITH NO DATA;
CREATE INDEX segment_sample_pair ON segment_sample (revision, from_stop, to_stop);

-- 모델 조회용 계정은 이 스키마를 읽기만 한다.
GRANT USAGE ON SCHEMA transit TO dllio_read;
GRANT SELECT ON ALL TABLES IN SCHEMA transit TO dllio_read;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA transit TO dllio_read;
ALTER ROLE dllio_read IN DATABASE dllio SET search_path = transit;
ALTER ROLE dllio_read IN DATABASE dllio SET default_transaction_read_only = on;
