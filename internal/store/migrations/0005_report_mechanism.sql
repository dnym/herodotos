-- 0005_report_mechanism.sql — a report normally arrives no earlier than
-- baseline travel allows. An adjudicated exceptional mechanism (signal
-- fires, a magical sending) may deliver sooner; it is recorded here so
-- the faster delivery stays explicit and inspectable.

ALTER TABLE reports ADD COLUMN mechanism TEXT;
