package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/3922510-creator/pic-tg/storage"
	"github.com/3922510-creator/pic-tg/telegram"
)

type downloadFlight struct {
	done chan struct{}
	data []byte
	mime string
	err  error
}

func (s *Server) uploadLimit() int64 { return telegram.MaxFileSize }

func (s *Server) activeConnection() (*telegram.Client, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.cfg.DefaultConnectionID
	return s.clients[id], id
}

func (s *Server) clientFor(id string) *telegram.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients[id]
}

func (s *Server) downloadStored(ctx context.Context, key string) ([]byte, string, error) {
	obj, err := s.db.GetObject(key)
	if err != nil {
		return nil, "", err
	}
	if obj == nil {
		return nil, "", errors.New("图片索引不存在")
	}
	return s.downloadObject(ctx, obj)
}

// downloadObject coalesces concurrent origin fetches for the same object key
// without caching image bytes in memory after the in-flight request completes.
func (s *Server) downloadObject(ctx context.Context, obj *storage.ObjectRecord) ([]byte, string, error) {
	if obj == nil {
		return nil, "", errors.New("图片索引不存在")
	}
	if obj.Backend != "telegram" {
		return nil, "", errors.New("图片存储来源无效")
	}
	client := s.clientFor(obj.ConnectionID)
	if client == nil {
		return nil, "", errors.New("TG 存储凭据未配置")
	}

	s.flightMu.Lock()
	if f, ok := s.flights[obj.Key]; ok {
		s.flightMu.Unlock()
		select {
		case <-f.done:
			return f.data, f.mime, f.err
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	f := &downloadFlight{done: make(chan struct{})}
	s.flights[obj.Key] = f
	s.flightMu.Unlock()

	go func() {
		fetchCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		data, err := client.Download(fetchCtx, obj.FID)
		if err == nil && int64(len(data)) != obj.Size {
			data = nil
			err = errors.New("TG 图片与索引大小不一致")
		}
		f.data = data
		f.mime = obj.Mime
		f.err = err
		s.flightMu.Lock()
		delete(s.flights, obj.Key)
		s.flightMu.Unlock()
		close(f.done)
	}()

	select {
	case <-f.done:
		return f.data, f.mime, f.err
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
}

func (s *Server) uploadStored(ctx context.Context, key string, raw []byte, origSize int64, width, height int, format string) (*storage.ObjectRecord, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 6 || parts[0] != "telegram" {
		return nil, errors.New("TG 图片路径无效")
	}
	connectionID := parts[1]
	client := s.clientFor(connectionID)
	if client == nil {
		return nil, errors.New("TG 接入配置不存在")
	}
	start := time.Now()
	select {
	case s.tgUploads <- struct{}{}:
		defer func() { <-s.tgUploads }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	existing, err := s.db.GetObject(key)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Backend != "telegram" {
			return nil, errors.New("图片存储来源冲突")
		}
		return existing, nil
	}
	mimeType := mime.TypeByExtension(filepath.Ext(key))
	if mimeType == "" {
		return nil, errors.New("无法识别图片 MIME 类型")
	}
	sending := time.Now()
	message, err := client.Upload(ctx, filepath.Base(key), mimeType, raw)
	if err != nil {
		return nil, err
	}
	sent := time.Now()
	obj := &storage.ObjectRecord{
		Key:          key,
		FID:          message.Document.FileID,
		FileID:       message.Document.UniqueID,
		Size:         int64(len(raw)),
		OrigSize:     origSize,
		Width:        width,
		Height:       height,
		Format:       format,
		Mime:         mimeType,
		CreatedAt:    time.Now().UTC().Format("2006-01-02 15:04:05"),
		Backend:      "telegram",
		ConnectionID: connectionID,
		TGChatID:     message.Chat.ID,
		TGMessageID:  message.ID,
	}
	if err := s.db.SaveObject(obj); err != nil {
		return nil, fmt.Errorf("TG 文件已保存但索引写入失败，请勿反复重传: %w", err)
	}
	log.Printf("storage_upload backend=telegram bytes=%d queue_ms=%d send_ms=%d index_ms=%d", len(raw), sending.Sub(start).Milliseconds(), sent.Sub(sending).Milliseconds(), time.Since(sent).Milliseconds())
	return obj, nil
}

func (s *Server) deleteStored(ctx context.Context, key string) error {
	obj, err := s.db.GetObject(key)
	if err != nil {
		return err
	}
	if obj == nil {
		return nil // Repeated deletion is already complete at the origin.
	}
	client := s.clientFor(obj.ConnectionID)
	if obj.Backend == "telegram" && client != nil {
		child, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if child.Err() == nil {
			select {
			case s.deleteSlots <- struct{}{}:
				defer func() { <-s.deleteSlots }()
				if err := client.Delete(child, obj.TGChatID, obj.TGMessageID); err != nil {
					log.Printf("storage_delete remote_failed key=%s; removing local index", key)
				}
			case <-child.Done():
				log.Printf("storage_delete remote_budget_exhausted key=%s; removing local index", key)
			}
		} else {
			log.Printf("storage_delete remote_budget_exhausted key=%s; removing local index", key)
		}
	} else {
		log.Printf("storage_delete remote_unavailable key=%s; removing local index", key)
	}
	// Request cancellation/remote timeout must not leave the remaining local
	// links live. No async job survives the response; DB failures remain errors.
	return s.db.DeleteObject(key)
}

func (s *Server) deleteBatch(ctx context.Context, keys []string) ([]string, []string) {
	// This is a shared remote budget for the complete batch, not per image.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	results := make([]error, len(keys))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for i := 0; i < min(3, len(keys)); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index] = s.deleteStored(ctx, keys[index])
			}
		}()
	}
	for index := range keys {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	deleted := make([]string, 0, len(keys))
	var errs []string
	for index, err := range results {
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", keys[index], err))
		} else {
			deleted = append(deleted, keys[index])
		}
	}
	return deleted, errs
}
