-- 0160 skipped a session_updated row that repeated the newest change_log row within 250 ms. Since
-- streamed text is committed once a second (three for a long row), and several chats interleave
-- their rows, that window no longer catches anything: on a live machine a streaming chat still
-- logged an identical row every 1.8 s. Each costs about 15 KB of database writes (the row and two
-- index pages): a measured two-minute stream wrote 2.89 MB with these rows and 1.96 MB without.
--
-- A streamed message or activity row is now skipped when it repeats its own session's newest
-- session_updated row exactly (same payload) within 10 s. Usage rows (usage_bindings) are left
-- alone: each one says the session's totals changed, which nothing else reports. Any real change
-- (activity state, termination, a review) has a different payload and is always written, so a
-- reader still hears every state change at once, and a long stream still produces a row every
-- 10 s. The Coding Tools chat reads a running card's activity every second regardless. The lookup
-- scans at most the newest 512 change_log rows (seq is the rowid), so a session quiet for longer
-- just writes its row. Times are compared to the second: 0160 cut them at 23 characters, and a
-- time stored on a whole second ("10:00:01 +0000 UTC", no fraction) became "10:00:01 +00", which
-- julianday cannot read, so such a repeat was always written. A timestamp that does not parse
-- makes the comparison NULL, so the row is written as before.

-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
DROP TRIGGER IF EXISTS conversation_messages_cdc_insert;
DROP TRIGGER IF EXISTS conversation_messages_cdc_update;
DROP TRIGGER IF EXISTS conversation_activities_cdc_insert;
DROP TRIGGER IF EXISTS conversation_activities_cdc_update;

CREATE TRIGGER conversation_messages_cdc_insert AFTER INSERT ON conversation_messages BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT e.project_id, e.session_id, 'session_updated', e.payload, e.created_at FROM (
    SELECT s.project_id AS project_id, s.id AS session_id, json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)) AS payload, NEW.updated_at AS created_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
    UNION ALL SELECT s.project_id,s.id,json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id
  ) e WHERE NOT EXISTS (SELECT 1 FROM change_log p WHERE p.seq = (SELECT MAX(l.seq) FROM change_log l WHERE l.seq > (SELECT MAX(seq) FROM change_log) - 512 AND l.session_id = e.session_id AND l.event_type = 'session_updated')
    AND p.payload = e.payload AND (julianday(substr(e.created_at,1,19)) - julianday(substr(p.created_at,1,19))) * 86400000.0 BETWEEN 0 AND 10000);
END;
CREATE TRIGGER conversation_messages_cdc_update AFTER UPDATE ON conversation_messages WHEN OLD.revision <> NEW.revision BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT e.project_id, e.session_id, 'session_updated', e.payload, e.created_at FROM (
    SELECT s.project_id AS project_id, s.id AS session_id, json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)) AS payload, NEW.updated_at AS created_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
    UNION ALL SELECT s.project_id,s.id,json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id
  ) e WHERE NOT EXISTS (SELECT 1 FROM change_log p WHERE p.seq = (SELECT MAX(l.seq) FROM change_log l WHERE l.seq > (SELECT MAX(seq) FROM change_log) - 512 AND l.session_id = e.session_id AND l.event_type = 'session_updated')
    AND p.payload = e.payload AND (julianday(substr(e.created_at,1,19)) - julianday(substr(p.created_at,1,19))) * 86400000.0 BETWEEN 0 AND 10000);
END;
CREATE TRIGGER conversation_activities_cdc_insert AFTER INSERT ON conversation_activities BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT e.project_id, e.session_id, 'session_updated', e.payload, e.created_at FROM (
    SELECT s.project_id AS project_id, s.id AS session_id, json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)) AS payload, NEW.updated_at AS created_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
    UNION ALL SELECT s.project_id,s.id,json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id
  ) e WHERE NOT EXISTS (SELECT 1 FROM change_log p WHERE p.seq = (SELECT MAX(l.seq) FROM change_log l WHERE l.seq > (SELECT MAX(seq) FROM change_log) - 512 AND l.session_id = e.session_id AND l.event_type = 'session_updated')
    AND p.payload = e.payload AND (julianday(substr(e.created_at,1,19)) - julianday(substr(p.created_at,1,19))) * 86400000.0 BETWEEN 0 AND 10000);
END;
CREATE TRIGGER conversation_activities_cdc_update AFTER UPDATE ON conversation_activities WHEN OLD.revision <> NEW.revision BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT e.project_id, e.session_id, 'session_updated', e.payload, e.created_at FROM (
    SELECT s.project_id AS project_id, s.id AS session_id, json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)) AS payload, NEW.updated_at AS created_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
    UNION ALL SELECT s.project_id,s.id,json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id
  ) e WHERE NOT EXISTS (SELECT 1 FROM change_log p WHERE p.seq = (SELECT MAX(l.seq) FROM change_log l WHERE l.seq > (SELECT MAX(seq) FROM change_log) - 512 AND l.session_id = e.session_id AND l.event_type = 'session_updated')
    AND p.payload = e.payload AND (julianday(substr(e.created_at,1,19)) - julianday(substr(p.created_at,1,19))) * 86400000.0 BETWEEN 0 AND 10000);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS conversation_messages_cdc_insert;
DROP TRIGGER IF EXISTS conversation_messages_cdc_update;
DROP TRIGGER IF EXISTS conversation_activities_cdc_insert;
DROP TRIGGER IF EXISTS conversation_activities_cdc_update;

CREATE TRIGGER conversation_messages_cdc_insert AFTER INSERT ON conversation_messages BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT e.project_id, e.session_id, 'session_updated', e.payload, e.created_at FROM (
    SELECT s.project_id AS project_id, s.id AS session_id, json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)) AS payload, NEW.updated_at AS created_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
    UNION ALL SELECT s.project_id,s.id,json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id
  ) e WHERE NOT EXISTS (SELECT 1 FROM change_log l WHERE l.seq = (SELECT MAX(seq) FROM change_log) AND l.session_id = e.session_id AND l.event_type = 'session_updated' AND l.payload = e.payload
    AND (julianday(substr(e.created_at,1,23)) - julianday(substr(l.created_at,1,23))) * 86400000.0 BETWEEN 0 AND 250);
END;
CREATE TRIGGER conversation_messages_cdc_update AFTER UPDATE ON conversation_messages WHEN OLD.revision <> NEW.revision BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT e.project_id, e.session_id, 'session_updated', e.payload, e.created_at FROM (
    SELECT s.project_id AS project_id, s.id AS session_id, json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)) AS payload, NEW.updated_at AS created_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
    UNION ALL SELECT s.project_id,s.id,json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id
  ) e WHERE NOT EXISTS (SELECT 1 FROM change_log l WHERE l.seq = (SELECT MAX(seq) FROM change_log) AND l.session_id = e.session_id AND l.event_type = 'session_updated' AND l.payload = e.payload
    AND (julianday(substr(e.created_at,1,23)) - julianday(substr(l.created_at,1,23))) * 86400000.0 BETWEEN 0 AND 250);
END;
CREATE TRIGGER conversation_activities_cdc_insert AFTER INSERT ON conversation_activities BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT e.project_id, e.session_id, 'session_updated', e.payload, e.created_at FROM (
    SELECT s.project_id AS project_id, s.id AS session_id, json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)) AS payload, NEW.updated_at AS created_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
    UNION ALL SELECT s.project_id,s.id,json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id
  ) e WHERE NOT EXISTS (SELECT 1 FROM change_log l WHERE l.seq = (SELECT MAX(seq) FROM change_log) AND l.session_id = e.session_id AND l.event_type = 'session_updated' AND l.payload = e.payload
    AND (julianday(substr(e.created_at,1,23)) - julianday(substr(l.created_at,1,23))) * 86400000.0 BETWEEN 0 AND 250);
END;
CREATE TRIGGER conversation_activities_cdc_update AFTER UPDATE ON conversation_activities WHEN OLD.revision <> NEW.revision BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT e.project_id, e.session_id, 'session_updated', e.payload, e.created_at FROM (
    SELECT s.project_id AS project_id, s.id AS session_id, json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)) AS payload, NEW.updated_at AS created_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
    UNION ALL SELECT s.project_id,s.id,json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id
  ) e WHERE NOT EXISTS (SELECT 1 FROM change_log l WHERE l.seq = (SELECT MAX(seq) FROM change_log) AND l.session_id = e.session_id AND l.event_type = 'session_updated' AND l.payload = e.payload
    AND (julianday(substr(e.created_at,1,23)) - julianday(substr(l.created_at,1,23))) * 86400000.0 BETWEEN 0 AND 250);
END;
-- +goose StatementEnd
