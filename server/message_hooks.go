package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
	"github.com/pkg/errors"
)

// Функция MessageHasBeenPosted вызывается сервером Mattermost после отправки сообщения.
// Проверяем, есть ли подходящий вебхук и если есть, то отправляем запрос
func (p *Plugin) MessageHasBeenPosted(c *plugin.Context, post *model.Post) {
	if post == nil {
		return
	}

	// Игнорируем системные сообщения и сообщения от ботов
	if strings.HasPrefix(post.Type, model.POST_SYSTEM_MESSAGE_PREFIX) {
		return
	}

	// Получаем автора сообщения
	user, err := p.API.GetUser(post.UserId)
	if err != nil {
		p.API.LogError("Failed to get user", "error", err.Error())
		return
	}

	// Получаем канал, в котором опубликовано сообщение
	channel, err := p.API.GetChannel(post.ChannelId)
	if err != nil {
		p.API.LogError("Failed to get channel", "error", err.Error())
		return
	}

	customWebhooks := p.getConfiguration().OutgoingWebhooks

	// Логируем полученные вебхуки (только при включённой отладке)
	if p.getConfiguration().Debug {
		p.API.LogDebug("Processing message for outgoing webhooks",
			"message", post.Message,
			"channel", channel.Name,
			"channel_type", channel.Type,
			"webhooks_count", len(customWebhooks))
	}

	// Обрабатываем каждый вебхук
	for _, wh := range customWebhooks {
		p.processWebhook(wh, post, user, channel)
	}
}

func (p *Plugin) processWebhook(wh *CustomOutgoingWebhook, post *model.Post, user *model.User, channel *model.Channel) {
	// Проверка, активен ли вебхук
	if !wh.Enabled {
		return
	}

	// Проверка канала, если указан
	if len(wh.ChannelIDs) != 0 && !contains(wh.ChannelIDs, post.ChannelId) {
		return
	}

	// Проверка триггерных слов
	triggerMatch, triggerWord := p.checkTriggerWords(wh, post.Message)
	if !triggerMatch {
		return
	}

	// Логируем (только при включённой отладке)
	if p.getConfiguration().Debug {
		p.API.LogDebug("Trigger Matched",
			"trigger_word", triggerWord,
			"check_bot_access", wh.CheckBotAccess)
	}

	if !p.checkAccessToChannel(wh, channel, triggerWord) {
		return
	}

	jsonData, err := p.createWebhookJson(wh, post, channel, user, triggerWord)
	if err != nil {
		p.API.LogError("Failed to generate JSON data",
			"webhook_id", wh.DisplayName,
			"error", err.Error())
		return
	}

	for _, callbackURL := range wh.CallbackURLs {
		// Отправка Webhook HTTP-запроса
		response, err := p.sendHTTPRequest(wh, callbackURL, jsonData)
		if err != nil {
			p.API.LogError("Failed to send HTTP request",
				"webhook_id", wh.DisplayName,
				"error", err.Error())
			continue
		}

		// Обработка ответа
		p.handleResponse(response, post, channel)
	}
}

func (p *Plugin) checkTriggerWords(wh *CustomOutgoingWebhook, message string) (bool, string) {
	if len(wh.TriggerWords) == 0 {
		return true, ""
	}

	messageLower := strings.ToLower(message)
	for _, triggerWord := range wh.TriggerWords {
		triggerWordLower := strings.ToLower(triggerWord)

		switch wh.TriggerWhen {
		case "startswith":
			if strings.HasPrefix(messageLower, triggerWordLower) {
				return true, triggerWord
			}
		case "exact":
			// Экранируем спецсимволы и ищем как отдельное слово
			pattern := `(^|[\s[:punct:]])` + regexp.QuoteMeta(triggerWordLower) + `($|[\s[:punct:]])`

			re, err := regexp.Compile(pattern)
			if err != nil {
				p.API.LogError("Failed to compile regexp for trigger word", "trigger word", triggerWord, "error", err)
				continue
			}
			if re.MatchString(messageLower) {
				return true, triggerWord
			}

			// Логируем (только при включённой отладке)
			if p.getConfiguration().Debug {
				p.API.LogDebug("Trigger word NOT matched",
					"message", message,
					"trigger_word", triggerWord,
					"pattern", pattern)
			}
		default:
			// По умолчанию проверяем, начинается ли сообщение с триггерного слова
			if strings.HasPrefix(messageLower, triggerWordLower) {
				return true, triggerWord
			}
		}
	}
	return false, ""
}

// Проверка прав бота, если triggerWord — это @упоминание
func (p *Plugin) checkAccessToChannel(wh *CustomOutgoingWebhook, channel *model.Channel, triggerWord string) bool {
	if !wh.CheckBotAccess {
		return true
	}

	if !strings.HasPrefix(triggerWord, "@") {
		p.API.LogError("Trigger word is not mention, webhook will not trigger",
			"trigger_word", triggerWord)
		return false
	}

	mentionedUsername := strings.TrimPrefix(triggerWord, "@")
	mentionedUser, appErr := p.API.GetUserByUsername(mentionedUsername)
	if appErr != nil {
		p.API.LogDebug("Failed to get user by trigger word mention, webhook will not trigger",
			"channel_id", channel.Id,
			"bot_username", mentionedUsername,
			"error", appErr.Error())
		return false
	}

	// Логируем (только при включённой отладке)
	if p.getConfiguration().Debug {
		p.API.LogDebug("User by trigger word mention found",
			"trigger_word", triggerWord,
			"mentionedUser", mentionedUser)
	}

	if mentionedUser.IsBot {
		// Проверяем, состоит ли бот в канале
		_, err := p.API.GetChannelMember(channel.Id, mentionedUser.Id)
		if err != nil {
			p.API.LogDebug("Bot is not a member of the channel, webhook will not trigger",
				"channel_id", channel.Id,
				"bot_username", mentionedUsername,
				"error", err.Error())
			return false
		}
	}

	return true
}

// Формируем данные для отправки WebHook
func (p *Plugin) createWebhookJson(wh *CustomOutgoingWebhook, post *model.Post, channel *model.Channel, user *model.User, triggerWord string) ([]byte, error) {
	// Получаем список email-адресов упоминаний
	mentionsNames := model.PossibleAtMentions(post.Message)
	mentionsEmail := make(map[string]string)
	for _, mentionedUsername := range mentionsNames {
		mentionedUser, appErr := p.API.GetUserByUsername(mentionedUsername)
		if appErr == nil && !mentionedUser.IsBot {
			_, err := p.API.GetChannelMember(channel.Id, mentionedUser.Id)
			if err != nil {
				p.API.LogDebug("User mention is not a member of the channel",
					"channel_id", channel.Id,
					"username", mentionedUsername,
					"error", err.Error())
				continue
			}
			mentionsEmail[mentionedUsername] = mentionedUser.Email
		}
	}

	// Получаем список ID файлов, прикреплённых к сообщению
	var fileIds []string
	if post.FileIds != nil {
		fileIds = post.FileIds
	}

	data := map[string]interface{}{
		"timestamp":     post.CreateAt,
		"user_id":       user.Id,
		"user_name":     user.Username,
		"channel_id":    post.ChannelId,
		"channel_name":  channel.Name,
		"team_id":       channel.TeamId,
		"post_id":       post.Id,
		"text":          post.Message,
		"trigger_word":  triggerWord,
		"token":         wh.Token,
		"mentionsEmail": mentionsEmail,
		"file_ids":      fileIds,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	return jsonData, nil
}

func (p *Plugin) sendHTTPRequest(wh *CustomOutgoingWebhook, callbackURL string, jsonData []byte) (*http.Response, error) {
	contentType := "application/x-www-form-urlencoded"
	if wh.ContentType == "application/json" {
		contentType = "application/json"
	}

	client := &http.Client{}
	req, err := http.NewRequest("POST", callbackURL, strings.NewReader(string(jsonData)))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create HTTP request")
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "Outgoing Webhook Enhancer/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send HTTP request")
	}

	return resp, nil
}

func (p *Plugin) handleResponse(resp *http.Response, post *model.Post, channel *model.Channel) {
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		p.API.LogWarn("Non-OK HTTP response",
			"status", resp.Status,
			"channel_id", channel.Id)
		return
	}

	// Декодируем JSON-ответ (ожидаем стандартную структуру Mattermost)
	var responseData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responseData); err != nil {
		p.API.LogDebug("Failed to decode JSON response", "error", err.Error())
		return
	}

	// Извлекаем текст для ответа
	text, ok := responseData["text"].(string)
	if !ok || text == "" {
		return
	}

	// Отправляем ответное сообщение
	responsePost := &model.Post{
		ChannelId: post.ChannelId,
		Message:   text,
		RootId:    post.Id, // Ответ в ту же ветку (thread)
		UserId:    post.UserId,
	}

	if _, err := p.API.CreatePost(responsePost); err != nil {
		p.API.LogError("Failed to create response post", "error", err.Error())
	}
}

// Generic-функция, работает с любыми типами (string, int и т.д.)
func contains[T comparable](slice []T, target T) bool {
	for _, v := range slice {
		if v == target {
			return true
		}
	}
	return false
}
