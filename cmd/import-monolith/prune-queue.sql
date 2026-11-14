-- Removes queued jobs whose URL is already archived by the press_media
-- import. Run against the hoarfrost database after the imported weeks are
-- reindexed (psql -1 -f prune-queue.sql). Jobs being captured right now
-- (leased) and URLs added by hand through the API or UI (source 'api') are
-- left alone. New polls need nothing: imported URLs are in the seen-URL
-- filter (archive_index.urls) once the weeks are indexed.

-- How many would go, per source:
SELECT j.source, count(*)
FROM app.jobs j
WHERE j.state = 'queued' AND j.source <> 'api'
  AND EXISTS (SELECT 1 FROM archive_index.urls u JOIN archive_index.pages p ON p.url_id = u.id
              WHERE u.storage = j.storage AND u.uri_md5 = md5(j.url)::uuid AND p.source = 'press_media')
GROUP BY j.source ORDER BY 2 DESC;

DELETE FROM app.jobs j
WHERE j.state = 'queued' AND j.source <> 'api'
  AND EXISTS (SELECT 1 FROM archive_index.urls u JOIN archive_index.pages p ON p.url_id = u.id
              WHERE u.storage = j.storage AND u.uri_md5 = md5(j.url)::uuid AND p.source = 'press_media');
