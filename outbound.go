package wechatbot

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Icatme/wechatbot-go/internal/markdown"
	"github.com/Icatme/wechatbot-go/internal/protocol"
)

// OutboundMessage is one item sent by one /sendmessage request.
// Set ClientID to reuse the same identity for an explicit retry.
type OutboundMessage struct {
	ClientID string
	RunID    string
	Item     MessageItem
	// Upload binds Item's uploaded resource to its original Bot, session and user.
	// BeforeSend may edit display metadata, but cannot remove or replace this binding.
	Upload *UploadResult
}

// SendResult identifies the outbound request. It is returned even when the
// transport fails so callers can retry with the same ClientID.
type SendResult struct {
	ClientID  string
	RunID     string
	MessageID MessageID // empty when the server omitted a receipt or the send failed
}

// SendRequest is passed to BeforeSend after ClientID generation and before the
// message is encoded. UserID and ClientID identify the validated destination
// and request and must not be changed by hooks.
type SendRequest struct {
	UserID  string
	Message OutboundMessage
}

// SendOutcome is passed to AfterSend after an HTTP send attempt.
type SendOutcome struct {
	Request SendRequest
	Result  SendResult
	Err     error
}

// ToolCallStatus is the terminal state reported for a tool call.
type ToolCallStatus string

const (
	ToolCallCompleted ToolCallStatus = "completed"
	ToolCallFailed    ToolCallStatus = "failed"
	ToolCallBlocked   ToolCallStatus = "blocked"
	ToolCallUnknown   ToolCallStatus = "unknown"
)

// NewToolCallStartMessage creates a tool-call progress message.
func NewToolCallStartMessage(runID, toolName, toolCallID string) OutboundMessage {
	completed := false
	return OutboundMessage{
		RunID: runID,
		Item: MessageItem{
			Type:         ItemToolCallStart,
			CreateTimeMs: time.Now().UnixMilli(),
			IsCompleted:  &completed,
			ToolCallStartItem: &ToolCallStartItem{
				ToolName:   toolName,
				ToolCallID: toolCallID,
			},
		},
	}
}

// NewToolCallResultMessage creates a terminal tool-call progress message.
func NewToolCallResultMessage(runID, toolName, toolCallID string, status ToolCallStatus) OutboundMessage {
	completed := true
	status = normalizeToolCallStatus(status)
	return OutboundMessage{
		RunID: runID,
		Item: MessageItem{
			Type:         ItemToolCallResult,
			CreateTimeMs: time.Now().UnixMilli(),
			IsCompleted:  &completed,
			ToolCallResultItem: &ToolCallResultItem{
				ToolName:   toolName,
				ToolCallID: toolCallID,
				Status:     string(status),
			},
		},
	}
}

func normalizeToolCallStatus(status ToolCallStatus) ToolCallStatus {
	switch status {
	case ToolCallCompleted, ToolCallFailed, ToolCallBlocked:
		return status
	default:
		return ToolCallUnknown
	}
}

// SendMessage sends exactly one message item to a user.
func (b *Bot) SendMessage(ctx context.Context, userID string, msg OutboundMessage) (SendResult, error) {
	_, sessionGeneration, err := b.readySessionForContext(ctx)
	if err != nil {
		return SendResult{}, err
	}
	if err := b.validateUpload(msg, sessionGeneration, userID); err != nil {
		return SendResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	contextToken := b.contextTokens.Get(userID)
	result, err := b.sendMessage(ctx, sessionGeneration, userID, contextToken, msg, nil)
	if err != nil {
		b.notifyError(ctx, sessionGeneration, userID, contextToken, err)
	}
	return result, err
}

// ReplyMessage replies with exactly one message item using an inbound message's
// context token.
func (b *Bot) ReplyMessage(ctx context.Context, inbound *IncomingMessage, msg OutboundMessage) (SendResult, error) {
	if inbound == nil {
		return SendResult{}, fmt.Errorf("inbound message is nil")
	}
	sessionGeneration, err := b.incomingSessionGeneration(inbound)
	if err != nil {
		return SendResult{}, err
	}
	if err := b.validateUpload(msg, sessionGeneration, inbound.UserID); err != nil {
		return SendResult{}, err
	}
	if err := requireContextToken(inbound.UserID, inbound.ContextToken); err != nil {
		return SendResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	if err := b.persistContextToken(sessionGeneration, inbound.UserID, inbound.ContextToken); err != nil {
		if errors.Is(err, ErrReauthRequired) {
			return SendResult{}, err
		}
		b.log("warn", "failed to persist context token: %v", err)
	}
	result, err := b.sendMessage(ctx, sessionGeneration, inbound.UserID, inbound.ContextToken, msg, nil)
	if err != nil {
		b.notifyError(ctx, sessionGeneration, inbound.UserID, inbound.ContextToken, err)
	}
	return result, err
}

func (b *Bot) sendMessage(ctx context.Context, sessionGeneration uint64, userID, contextToken string, msg OutboundMessage, mediaData []byte) (SendResult, error) {
	if err := requireContextToken(userID, contextToken); err != nil {
		return SendResult{}, err
	}
	creds, err := b.readySession(sessionGeneration)
	if err != nil {
		return SendResult{}, err
	}
	if err := b.validateUpload(msg, sessionGeneration, userID); err != nil {
		return SendResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	// Hooks may edit one attempt, but cannot mutate the caller's reusable message
	// graph or upload. Preserve the immutable private binding identity as well
	// as the wrapper identity: assigning another UploadResult through the wrapper
	// must not silently replace the resource that was validated above.
	msg.Item = cloneMessageItemGraph(msg.Item)
	var validatedUploadBinding *uploadBinding
	if msg.Upload != nil {
		copy := *msg.Upload
		copy.AESKey = bytes.Clone(copy.AESKey)
		msg.Upload = &copy
		validatedUploadBinding = copy.binding
	}
	if msg.ClientID == "" {
		clientID, err := newClientID()
		if err != nil {
			return SendResult{}, err
		}
		msg.ClientID = clientID
	}

	validatedUserID := userID
	originalMedia := quotedMediaFromItem(&msg.Item)
	validatedClientID := msg.ClientID
	request := SendRequest{UserID: userID, Message: msg}
	result := SendResult{ClientID: msg.ClientID, RunID: msg.RunID}
	if err := b.hooks.BeforeSend.Run(&request); err != nil {
		return result, fmt.Errorf("BeforeSend hook failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if request.UserID != validatedUserID {
		return result, fmt.Errorf("BeforeSend hook cannot change user ID")
	}
	if request.Message.ClientID != validatedClientID {
		return result, fmt.Errorf("BeforeSend hook cannot change client ID")
	}
	if request.Message.Upload != msg.Upload || (request.Message.Upload != nil && request.Message.Upload.binding != validatedUploadBinding) {
		return result, fmt.Errorf("BeforeSend hook cannot change upload binding")
	}
	if err := b.validateUpload(request.Message, sessionGeneration, userID); err != nil {
		return result, err
	}
	request.Message.Item = cloneMessageItemGraph(request.Message.Item)
	normalizeMessageItemText(&request.Message.Item, make(map[*MessageItem]struct{}))
	if err := validateOutboundMessage(request.Message); err != nil {
		return result, err
	}
	result.RunID = request.Message.RunID
	wire := protocol.BuildMessage(
		request.UserID,
		contextToken,
		request.Message.ClientID,
		request.Message.RunID,
		request.Message.Item,
	)
	receipt, token, sendErr := authenticatedRequestResult(b, ctx, sessionGeneration, func(requestContext context.Context, baseURL, token string) (*protocol.SendMessageResponse, error) {
		return b.client.SendMessage(requestContext, baseURL, token, wire)
	})
	if token == "" {
		return result, sendErr
	}
	sendErr = b.handleAuthenticatedErrorForSession(sessionGeneration, token, sendErr)
	if sendErr == nil && receipt != nil {
		result.MessageID = receipt.MessageID
		// Cache exactly the post-hook, normalized text that was accepted. Cache
		// failures are reported separately: returning a send error here could
		// cause a caller to deliver the already accepted message again.
		b.cacheQuote(creds.AccountID, request.UserID, receipt.MessageID, quoteText([]MessageItem{request.Message.Item}))
		acceptedMedia := quotedMediaFromItem(&request.Message.Item)
		if acceptedMedia != nil {
			var retained []byte
			if reflect.DeepEqual(originalMedia, acceptedMedia) {
				retained = mediaData
			}
			if retained != nil {
				acceptedMedia.MIMEType = downloadedMIME(&DownloadedMedia{Data: retained, Type: string(acceptedMedia.Type), FileName: acceptedMedia.FileName})
			}
			b.cacheQuoteMedia(creds.AccountID, request.UserID, receipt.MessageID, acceptedMedia, retained)
		}
	}
	outcome := SendOutcome{Request: request, Result: result, Err: sendErr}
	if hookErr := b.hooks.AfterSend.Run(outcome); hookErr != nil {
		b.reportError(fmt.Errorf("AfterSend hook failed: %w", hookErr))
	}
	return result, sendErr
}

func (b *Bot) validateUpload(msg OutboundMessage, generation uint64, userID string) error {
	if msg.Upload == nil {
		return nil
	}
	bound := msg.Upload.binding
	if bound == nil || bound.bot != b || bound.userID != userID {
		return fmt.Errorf("upload does not belong to this Bot and recipient")
	}
	if bound.generation != generation {
		return ErrSessionChanged
	}
	var media, thumb *CDNMedia
	switch {
	case bound.mediaType == int(MediaImage) && msg.Item.Type == ItemImage && msg.Item.ImageItem != nil:
		if msg.Item.ImageItem.AESKey != "" || msg.Item.ImageItem.URL != "" {
			return fmt.Errorf("uploaded image resource cannot be replaced")
		}
		media, thumb = msg.Item.ImageItem.Media, msg.Item.ImageItem.ThumbMedia
	case bound.mediaType == int(MediaVideo) && msg.Item.Type == ItemVideo && msg.Item.VideoItem != nil:
		media, thumb = msg.Item.VideoItem.Media, msg.Item.VideoItem.ThumbMedia
	case bound.mediaType == int(MediaFile) && msg.Item.Type == ItemFile && msg.Item.FileItem != nil:
		media = msg.Item.FileItem.Media
	case bound.mediaType == int(MediaVoice) && msg.Item.Type == ItemVoice && msg.Item.VoiceItem != nil:
		media = msg.Item.VoiceItem.Media
	}
	if media == nil || *media != bound.media || (thumb != nil && *thumb != bound.thumb) || (thumb == nil && bound.thumb != (CDNMedia{})) {
		return fmt.Errorf("message does not contain the bound uploaded resource")
	}
	return nil
}

func cloneMessageItemGraph(item MessageItem) MessageItem {
	clone := item
	cloneMessageItemPointers(&clone, &item, make(map[*MessageItem]*MessageItem))
	return clone
}

func cloneMessageItemPointer(item *MessageItem, visited map[*MessageItem]*MessageItem) *MessageItem {
	if item == nil {
		return nil
	}
	if clone, ok := visited[item]; ok {
		return clone
	}
	clone := new(MessageItem)
	visited[item] = clone
	*clone = *item
	cloneMessageItemPointers(clone, item, visited)
	return clone
}

func cloneMessageItemPointers(clone, item *MessageItem, visited map[*MessageItem]*MessageItem) {
	if item.IsCompleted != nil {
		v := *item.IsCompleted
		clone.IsCompleted = &v
	}
	if item.TextItem != nil {
		textItem := *item.TextItem
		clone.TextItem = &textItem
	}
	if p := item.ImageItem; p != nil {
		v := *p
		v.Media, v.ThumbMedia = cloneOutboundCDN(p.Media), cloneOutboundCDN(p.ThumbMedia)
		clone.ImageItem = &v
	}
	if p := item.VoiceItem; p != nil {
		v := *p
		v.Media = cloneOutboundCDN(p.Media)
		clone.VoiceItem = &v
	}
	if p := item.FileItem; p != nil {
		v := *p
		v.Media = cloneOutboundCDN(p.Media)
		clone.FileItem = &v
	}
	if p := item.VideoItem; p != nil {
		v := *p
		v.Media, v.ThumbMedia = cloneOutboundCDN(p.Media), cloneOutboundCDN(p.ThumbMedia)
		clone.VideoItem = &v
	}
	if p := item.ToolCallStartItem; p != nil {
		v := *p
		clone.ToolCallStartItem = &v
	}
	if p := item.ToolCallResultItem; p != nil {
		v := *p
		clone.ToolCallResultItem = &v
	}
	if item.RefMsg != nil {
		refMsg := *item.RefMsg
		refMsg.MessageItem = cloneMessageItemPointer(item.RefMsg.MessageItem, visited)
		if refMsg.PartialText != nil {
			v := *refMsg.PartialText
			refMsg.PartialText = &v
		}
		clone.RefMsg = &refMsg
	}
}

func cloneOutboundCDN(media *CDNMedia) *CDNMedia {
	if media == nil {
		return nil
	}
	copy := *media
	return &copy
}

func normalizeMessageItemText(item *MessageItem, visited map[*MessageItem]struct{}) {
	if item == nil {
		return
	}
	if _, ok := visited[item]; ok {
		return
	}
	visited[item] = struct{}{}

	if item.TextItem != nil {
		item.TextItem.Text = markdown.NormalizeBareLessThan(item.TextItem.Text)
	}
	if item.RefMsg != nil {
		item.RefMsg.Title = markdown.NormalizeBareLessThan(item.RefMsg.Title)
		normalizeMessageItemText(item.RefMsg.MessageItem, visited)
	}
}

func validateOutboundMessage(msg OutboundMessage) error {
	if msg.ClientID == "" {
		return fmt.Errorf("outbound message client ID is empty")
	}
	switch msg.Item.Type {
	case ItemText:
		if msg.Item.TextItem == nil {
			return fmt.Errorf("text message is missing text_item")
		}
	case ItemImage:
		if msg.Item.ImageItem == nil {
			return fmt.Errorf("image message is missing image_item")
		}
	case ItemVoice:
		if msg.Item.VoiceItem == nil {
			return fmt.Errorf("voice message is missing voice_item")
		}
	case ItemFile:
		if msg.Item.FileItem == nil {
			return fmt.Errorf("file message is missing file_item")
		}
	case ItemVideo:
		if msg.Item.VideoItem == nil {
			return fmt.Errorf("video message is missing video_item")
		}
	case ItemToolCallStart:
		if msg.Item.ToolCallStartItem == nil {
			return fmt.Errorf("tool-call start message is missing tool_call_start_item")
		}
	case ItemToolCallResult:
		if msg.Item.ToolCallResultItem == nil {
			return fmt.Errorf("tool-call result message is missing tool_call_result_item")
		}
	default:
		return fmt.Errorf("unsupported outbound message item type %d", msg.Item.Type)
	}
	return nil
}

func newClientID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate client ID: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}
