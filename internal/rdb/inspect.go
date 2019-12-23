package rdb

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-redis/redis/v7"
	"github.com/hibiken/asynq/internal/base"
	"github.com/rs/xid"
)

// Stats represents a state of queues at a certain time.
type Stats struct {
	Enqueued   int
	InProgress int
	Scheduled  int
	Retry      int
	Dead       int
	Timestamp  time.Time
}

// EnqueuedTask is a task in a queue and is ready to be processed.
// Note: This is read only and used for monitoring purpose.
type EnqueuedTask struct {
	ID      xid.ID
	Type    string
	Payload map[string]interface{}
}

// InProgressTask is a task that's currently being processed.
// Note: This is read only and used for monitoring purpose.
type InProgressTask struct {
	ID      xid.ID
	Type    string
	Payload map[string]interface{}
}

// ScheduledTask is a task that's scheduled to be processed in the future.
// Note: This is read only and used for monitoring purpose.
type ScheduledTask struct {
	ID        xid.ID
	Type      string
	Payload   map[string]interface{}
	ProcessAt time.Time
	Score     int64
}

// RetryTask is a task that's in retry queue because worker failed to process the task.
// Note: This is read only and used for monitoring purpose.
type RetryTask struct {
	ID      xid.ID
	Type    string
	Payload map[string]interface{}
	// TODO(hibiken): add LastFailedAt time.Time
	ProcessAt time.Time
	ErrorMsg  string
	Retried   int
	Retry     int
	Score     int64
}

// DeadTask is a task in that has exhausted all retries.
// Note: This is read only and used for monitoring purpose.
type DeadTask struct {
	ID           xid.ID
	Type         string
	Payload      map[string]interface{}
	LastFailedAt time.Time
	ErrorMsg     string
	Score        int64
}

// CurrentStats returns a current state of the queues.
func (r *RDB) CurrentStats() (*Stats, error) {
	pipe := r.client.Pipeline()
	qlen := pipe.LLen(base.DefaultQueue)
	plen := pipe.LLen(base.InProgressQueue)
	slen := pipe.ZCard(base.ScheduledQueue)
	rlen := pipe.ZCard(base.RetryQueue)
	dlen := pipe.ZCard(base.DeadQueue)
	_, err := pipe.Exec()
	if err != nil {
		return nil, err
	}
	return &Stats{
		Enqueued:   int(qlen.Val()),
		InProgress: int(plen.Val()),
		Scheduled:  int(slen.Val()),
		Retry:      int(rlen.Val()),
		Dead:       int(dlen.Val()),
		Timestamp:  time.Now(),
	}, nil
}

// RedisInfo returns a map of redis info.
func (r *RDB) RedisInfo() (map[string]string, error) {
	res, err := r.client.Info().Result()
	if err != nil {
		return nil, err
	}
	info := make(map[string]string)
	lines := strings.Split(res, "\r\n")
	for _, l := range lines {
		kv := strings.Split(l, ":")
		if len(kv) == 2 {
			info[kv[0]] = kv[1]
		}
	}
	return info, nil
}

// ListEnqueued returns all enqueued tasks that are ready to be processed.
func (r *RDB) ListEnqueued() ([]*EnqueuedTask, error) {
	data, err := r.client.LRange(base.DefaultQueue, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var tasks []*EnqueuedTask
	for _, s := range data {
		var msg base.TaskMessage
		err := json.Unmarshal([]byte(s), &msg)
		if err != nil {
			// continue // bad data, ignore and continue
			return nil, err
		}
		tasks = append(tasks, &EnqueuedTask{
			ID:      msg.ID,
			Type:    msg.Type,
			Payload: msg.Payload,
		})
	}
	return tasks, nil
}

// ListInProgress returns all tasks that are currently being processed.
func (r *RDB) ListInProgress() ([]*InProgressTask, error) {
	data, err := r.client.LRange(base.InProgressQueue, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var tasks []*InProgressTask
	for _, s := range data {
		var msg base.TaskMessage
		err := json.Unmarshal([]byte(s), &msg)
		if err != nil {
			continue // bad data, ignore and continue
		}
		tasks = append(tasks, &InProgressTask{
			ID:      msg.ID,
			Type:    msg.Type,
			Payload: msg.Payload,
		})
	}
	return tasks, nil
}

// ListScheduled returns all tasks that are scheduled to be processed
// in the future.
func (r *RDB) ListScheduled() ([]*ScheduledTask, error) {
	data, err := r.client.ZRangeWithScores(base.ScheduledQueue, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var tasks []*ScheduledTask
	for _, z := range data {
		s, ok := z.Member.(string)
		if !ok {
			continue // bad data, ignore and continue
		}
		var msg base.TaskMessage
		err := json.Unmarshal([]byte(s), &msg)
		if err != nil {
			continue // bad data, ignore and continue
		}
		processAt := time.Unix(int64(z.Score), 0)
		tasks = append(tasks, &ScheduledTask{
			ID:        msg.ID,
			Type:      msg.Type,
			Payload:   msg.Payload,
			ProcessAt: processAt,
			Score:     int64(z.Score),
		})
	}
	return tasks, nil
}

// ListRetry returns all tasks that have failed before and willl be retried
// in the future.
func (r *RDB) ListRetry() ([]*RetryTask, error) {
	data, err := r.client.ZRangeWithScores(base.RetryQueue, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var tasks []*RetryTask
	for _, z := range data {
		s, ok := z.Member.(string)
		if !ok {
			continue // bad data, ignore and continue
		}
		var msg base.TaskMessage
		err := json.Unmarshal([]byte(s), &msg)
		if err != nil {
			continue // bad data, ignore and continue
		}
		processAt := time.Unix(int64(z.Score), 0)
		tasks = append(tasks, &RetryTask{
			ID:        msg.ID,
			Type:      msg.Type,
			Payload:   msg.Payload,
			ErrorMsg:  msg.ErrorMsg,
			Retry:     msg.Retry,
			Retried:   msg.Retried,
			ProcessAt: processAt,
			Score:     int64(z.Score),
		})
	}
	return tasks, nil
}

// ListDead returns all tasks that have exhausted its retry limit.
func (r *RDB) ListDead() ([]*DeadTask, error) {
	data, err := r.client.ZRangeWithScores(base.DeadQueue, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var tasks []*DeadTask
	for _, z := range data {
		s, ok := z.Member.(string)
		if !ok {
			continue // bad data, ignore and continue
		}
		var msg base.TaskMessage
		err := json.Unmarshal([]byte(s), &msg)
		if err != nil {
			continue // bad data, ignore and continue
		}
		lastFailedAt := time.Unix(int64(z.Score), 0)
		tasks = append(tasks, &DeadTask{
			ID:           msg.ID,
			Type:         msg.Type,
			Payload:      msg.Payload,
			ErrorMsg:     msg.ErrorMsg,
			LastFailedAt: lastFailedAt,
			Score:        int64(z.Score),
		})
	}
	return tasks, nil
}

// EnqueueDeadTask finds a task that matches the given id and score from dead queue
// and enqueues it for processing. If a task that matches the id and score
// does not exist, it returns ErrTaskNotFound.
func (r *RDB) EnqueueDeadTask(id xid.ID, score int64) error {
	n, err := r.removeAndEnqueue(base.DeadQueue, id.String(), float64(score))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// EnqueueRetryTask finds a task that matches the given id and score from retry queue
// and enqueues it for processing. If a task that matches the id and score
// does not exist, it returns ErrTaskNotFound.
func (r *RDB) EnqueueRetryTask(id xid.ID, score int64) error {
	n, err := r.removeAndEnqueue(base.RetryQueue, id.String(), float64(score))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// EnqueueScheduledTask finds a task that matches the given id and score from scheduled queue
// and enqueues it for processing. If a task that matches the id and score does not
// exist, it returns ErrTaskNotFound.
func (r *RDB) EnqueueScheduledTask(id xid.ID, score int64) error {
	n, err := r.removeAndEnqueue(base.ScheduledQueue, id.String(), float64(score))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// EnqueueAllScheduledTasks enqueues all tasks from scheduled queue
// and returns the number of tasks enqueued.
func (r *RDB) EnqueueAllScheduledTasks() (int64, error) {
	return r.removeAndEnqueueAll(base.ScheduledQueue)
}

// EnqueueAllRetryTasks enqueues all tasks from retry queue
// and returns the number of tasks enqueued.
func (r *RDB) EnqueueAllRetryTasks() (int64, error) {
	return r.removeAndEnqueueAll(base.RetryQueue)
}

// EnqueueAllDeadTasks enqueues all tasks from dead queue
// and returns the number of tasks enqueued.
func (r *RDB) EnqueueAllDeadTasks() (int64, error) {
	return r.removeAndEnqueueAll(base.DeadQueue)
}

func (r *RDB) removeAndEnqueue(zset, id string, score float64) (int64, error) {
	script := redis.NewScript(`
	local msgs = redis.call("ZRANGEBYSCORE", KEYS[1], ARGV[1], ARGV[1])
	for _, msg in ipairs(msgs) do
		local decoded = cjson.decode(msg)
		if decoded["ID"] == ARGV[2] then
			redis.call("ZREM", KEYS[1], msg)
			redis.call("LPUSH", KEYS[2], msg)
			return 1
		end
	end
	return 0
	`)
	res, err := script.Run(r.client, []string{zset, base.DefaultQueue}, score, id).Result()
	if err != nil {
		return 0, err
	}
	n, ok := res.(int64)
	if !ok {
		return 0, fmt.Errorf("could not cast %v to int64", res)
	}
	return n, nil
}

func (r *RDB) removeAndEnqueueAll(zset string) (int64, error) {
	script := redis.NewScript(`
	local msgs = redis.call("ZRANGE", KEYS[1], 0, -1)
	for _, msg in ipairs(msgs) do
		redis.call("ZREM", KEYS[1], msg)
		redis.call("LPUSH", KEYS[2], msg)
	end
	return table.getn(msgs)
	`)
	res, err := script.Run(r.client, []string{zset, base.DefaultQueue}).Result()
	if err != nil {
		return 0, err
	}
	n, ok := res.(int64)
	if !ok {
		return 0, fmt.Errorf("could not cast %v to int64", res)
	}
	return n, nil
}

// DeleteDeadTask finds a task that matches the given id and score from dead queue
// and deletes it. If a task that matches the id and score does not exist,
// it returns ErrTaskNotFound.
func (r *RDB) DeleteDeadTask(id xid.ID, score int64) error {
	return r.deleteTask(base.DeadQueue, id.String(), float64(score))
}

// DeleteRetryTask finds a task that matches the given id and score from retry queue
// and deletes it. If a task that matches the id and score does not exist,
// it returns ErrTaskNotFound.
func (r *RDB) DeleteRetryTask(id xid.ID, score int64) error {
	return r.deleteTask(base.RetryQueue, id.String(), float64(score))
}

// DeleteScheduledTask finds a task that matches the given id and score from
// scheduled queue  and deletes it. If a task that matches the id and score
//does not exist, it returns ErrTaskNotFound.
func (r *RDB) DeleteScheduledTask(id xid.ID, score int64) error {
	return r.deleteTask(base.ScheduledQueue, id.String(), float64(score))
}

func (r *RDB) deleteTask(zset, id string, score float64) error {
	script := redis.NewScript(`
	local msgs = redis.call("ZRANGEBYSCORE", KEYS[1], ARGV[1], ARGV[1])
	for _, msg in ipairs(msgs) do
		local decoded = cjson.decode(msg)
		if decoded["ID"] == ARGV[2] then
			redis.call("ZREM", KEYS[1], msg)
			return 1
		end
	end
	return 0
	`)
	res, err := script.Run(r.client, []string{zset}, score, id).Result()
	if err != nil {
		return err
	}
	n, ok := res.(int64)
	if !ok {
		return fmt.Errorf("could not cast %v to int64", res)
	}
	if n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// DeleteAllDeadTasks deletes all tasks from the dead queue.
func (r *RDB) DeleteAllDeadTasks() error {
	return r.client.Del(base.DeadQueue).Err()
}

// DeleteAllRetryTasks deletes all tasks from the dead queue.
func (r *RDB) DeleteAllRetryTasks() error {
	return r.client.Del(base.RetryQueue).Err()
}

// DeleteAllScheduledTasks deletes all tasks from the dead queue.
func (r *RDB) DeleteAllScheduledTasks() error {
	return r.client.Del(base.ScheduledQueue).Err()
}
