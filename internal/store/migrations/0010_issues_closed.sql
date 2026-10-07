-- Similar closed issues scan the most recently closed issues' titles, so
-- an index on (status, closed_at) makes that a range scan.

CREATE INDEX issues_closed ON issues (status, closed_at);
