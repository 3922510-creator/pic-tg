package storage

import "regexp"

// KeyPattern validates canonical Telegram-backed object keys.
var KeyPattern = regexp.MustCompile(`^telegram(/[a-f0-9]{16})?/[0-9]{4}/[0-9]{2}/[0-9]{2}/[a-f0-9]{12,32}\.(png|jpg|jpeg|gif|webp|avif)$`)
