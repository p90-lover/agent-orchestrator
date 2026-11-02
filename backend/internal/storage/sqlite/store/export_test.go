package store

import "context"

// ListPreviewSessionsSQL lets the external tests check the query plan of the
// exact statement the preview poller runs.
const ListPreviewSessionsSQL = listPreviewSessionsSQL

// ActivityPageKeysForTest runs activityPageKeysSQL, for comparing it with the
// generated SelectConversationActivitiesPage it mirrors.
func (s *Store) ActivityPageKeysForTest(ctx context.Context, conversationID string, beforeSequence, limit int64) ([]string, error) {
	rows, err := s.readDB.QueryContext(ctx, activityPageKeysSQL, conversationID, beforeSequence, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		var revision int64
		if err := rows.Scan(&id, &revision); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
