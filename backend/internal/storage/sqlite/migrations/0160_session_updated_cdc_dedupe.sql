-- Streaming a reply updates its message and activity rows several times a second, and each
-- update fanned out a session_updated change_log row. While a turn runs those rows are
-- identical (same session, same payload: the activity state and termination flag have not
-- changed), so 84% of change_log was an exact repeat of the row before it, written within
-- 250 ms of it: a database write per streamed chunk for nothing a reader can tell apart.
--
-- The triggers now skip a row that repeats the newest change_log row exactly (same session,
-- event and payload) within 250 ms. Any real change (activity state, termination, a turn's
-- state, a review) has a different payload and is always written, and a reader that refetches
-- on a session_updated event still gets one per burst. A timestamp that does not parse makes
-- the comparison NULL, so the row is written as before.

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

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS conversation_messages_cdc_insert;
DROP TRIGGER IF EXISTS conversation_messages_cdc_update;
DROP TRIGGER IF EXISTS conversation_activities_cdc_insert;
DROP TRIGGER IF EXISTS conversation_activities_cdc_update;

CREATE TRIGGER conversation_messages_cdc_insert AFTER INSERT ON conversation_messages BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id, s.id, 'session_updated', json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)), NEW.updated_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
  UNION ALL SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id;
END;
CREATE TRIGGER conversation_messages_cdc_update AFTER UPDATE ON conversation_messages WHEN OLD.revision <> NEW.revision BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
  UNION ALL SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id;
END;
CREATE TRIGGER conversation_activities_cdc_insert AFTER INSERT ON conversation_activities BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
  UNION ALL SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id;
END;
CREATE TRIGGER conversation_activities_cdc_update AFTER UPDATE ON conversation_activities WHEN OLD.revision <> NEW.revision BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
  UNION ALL SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
