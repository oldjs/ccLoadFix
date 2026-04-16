package app

import (
	"context"
	"log"
	"time"

	"ccLoad/internal/cooldown"
)

// cooldownWriteTask 冷却写入任务
type cooldownWriteTask struct {
	input     cooldown.ErrorInput // 错误路径：HandleError 的输入
	channelID int64               // 成功路径：清除冷却的渠道ID
	keyIndex  int                 // 成功路径：清除冷却的Key索引（-1表示无Key）
	isSuccess bool                // true=清除冷却, false=触发冷却
}

// cooldownWriteWorker 后台worker，处理冷却DB写入
// 请求路径只做分类（DecideAction），DB持久化在这里异步完成
func (s *Server) cooldownWriteWorker() {
	defer s.wg.Done()

	for {
		select {
		case task := <-s.cooldownWriteCh:
			s.processCooldownWrite(task)
		case <-s.shutdownCh:
			s.drainCooldownQueue()
			return
		}
	}
}

// processCooldownWrite 执行单个冷却写入任务
func (s *Server) processCooldownWrite(task cooldownWriteTask) {
	ctx, cancel := context.WithTimeout(context.Background(), cooldownWriteTimeout)
	defer cancel()

	if task.isSuccess {
		// 成功路径：清除冷却
		if err := s.cooldownManager.ClearChannelCooldown(ctx, task.channelID); err != nil {
			count := cooldownClearChannelFailCount.Add(1)
			if count%100 == 1 {
				log.Printf("[WARN] async ClearChannelCooldown failed (total: %d): channel_id=%d err=%v", count, task.channelID, err)
			}
		}
		if task.keyIndex >= 0 {
			if err := s.cooldownManager.ClearKeyCooldown(ctx, task.channelID, task.keyIndex); err != nil {
				count := cooldownClearKeyFailCount.Add(1)
				if count%100 == 1 {
					log.Printf("[WARN] async ClearKeyCooldown failed (total: %d): channel_id=%d key_index=%d err=%v", count, task.channelID, task.keyIndex, err)
				}
			}
		}
	} else {
		// 错误路径：HandleError 会重新分类+写DB
		s.cooldownManager.HandleError(ctx, task.input)
	}
}

// drainCooldownQueue 关闭前排空队列里的任务
func (s *Server) drainCooldownQueue() {
	deadline := time.After(2 * time.Second)
	for {
		select {
		case task := <-s.cooldownWriteCh:
			s.processCooldownWrite(task)
		case <-deadline:
			remaining := len(s.cooldownWriteCh)
			if remaining > 0 {
				log.Printf("[WARN] cooldown write queue drained with %d tasks remaining", remaining)
			}
			return
		default:
			return
		}
	}
}

// queueCooldownWrite 投递冷却写入任务（非阻塞，队列满时丢弃）
func (s *Server) queueCooldownWrite(task cooldownWriteTask) {
	select {
	case s.cooldownWriteCh <- task:
	default:
		count := s.cooldownWriteDropCount.Add(1)
		if count%100 == 1 {
			log.Printf("[WARN] cooldown write queue full, task dropped (total: %d)", count)
		}
	}
}
