-- Reconcile task.notifiers with the channels 0005 introduced.
--
-- Before 0005 a task's notifiers named plugin TYPES and were validated against
-- the plugin registry, so "telegram" and "webhook" were accepted there. They now
-- name configured channel rows, and 0005 seeds exactly one of those ("log"). A
-- task still carrying a type name is therefore rejected by every write with
-- "unknown notifier channel" — it cannot be edited, retimed or even paused until
-- someone deselects the name by hand.
--
-- Dropping those names loses no delivery: a channel built from the registry with
-- a nil config could never be constructed for telegram or webhook, which is the
-- gap 0005 exists to close. An operator who wants them back creates the channel
-- with a real config and selects it on the task.
UPDATE task
   SET notifiers = (SELECT json_group_array(j.value)
                      FROM json_each(task.notifiers) j
                     WHERE j.value IN (SELECT name FROM notifier))
 WHERE EXISTS (SELECT 1 FROM json_each(task.notifiers) j
                WHERE j.value NOT IN (SELECT name FROM notifier));
