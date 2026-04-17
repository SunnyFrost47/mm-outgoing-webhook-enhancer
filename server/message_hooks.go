package main // Функция MessageHasBeenPosted вызывается сервером Mattermost после отправки сообщения.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
	"github.com/pkg/errors"
)

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

	// Получаем список исходящих вебхуков и кешируем
	if (p.outgoingWebhooks == nil) || (len(p.outgoingWebhooks[channel.TeamId]) == 0) {
		hooks, err := p.getOutgoingWebhooks(channel.TeamId)

		if err != nil {
			p.API.LogError("Failed to get outgoing webhooks", "error", err.Error())
			return
		}

		p.outgoingWebhooks[channel.TeamId] = hooks
	}

	outWebhooksByTeam := p.outgoingWebhooks[channel.TeamId]

	// Логируем полученные вебхуки (только при включённой отладке)
	if p.getConfiguration().Debug {
		p.API.LogDebug("Processing message for outgoing webhooks",
			"message", post.Message,
			"channel", channel.Name,
			"channel_type", channel.Type,
			"webhooks_count", len(outWebhooksByTeam))
	}

	// Обрабатываем каждый вебхук
	for _, wh := range outWebhooksByTeam {
		p.processWebhook(wh, post, user, channel)
	}
}

// GetOutgoingWebhooks возвращает список исходящих вебхуков
func (p *Plugin) getOutgoingWebhooks(teamId string) ([]*model.OutgoingWebhook, *model.AppError) {
	// Инициализация клиента API v4
	siteURL := *p.API.GetConfig().ServiceSettings.SiteURL
	if siteURL == "" {
		return nil, model.NewAppError("getOutgoingWebhooks", "plugin.site_url_empty", nil, "SiteURL is empty", 500)
	}
	client := model.NewAPIv4Client(siteURL)

	// Установка токена бота
	botToken := "yt4mzkm4tbgqurmxr4ocg7q37c"
	client.SetToken(botToken)

	/*myClient := &http.Client{}
	req, _ := http.NewRequest("GET", siteURL+"/api/v4/hooks/outgoing?per_page=100&team_id="+teamId, nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	respHttp, err := myClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "DEBUG: getOutgoingWebhooks err: %v\n", err)
		return nil, model.NewAppError("getOutgoingWebhooks", "plugin.site_url_empty", nil, "SiteURL is empty", 500)
	}
	body, _ := io.ReadAll(respHttp.Body)
	fmt.Fprintf(os.Stderr, "DEBUG: getOutgoingWebhooks response: %v\n", body)
	if respHttp.StatusCode != 200 {
		p.API.LogError("HTTP error", "status", respHttp.StatusCode, "body", string(body))
		return nil, model.NewAppError("getOutgoingWebhooks", "plugin.site_url_empty", nil, "SiteURL is empty", 500)
	}*/

	// Получение списка исходящих вебхуков
	hooks, resp := client.GetOutgoingWebhooks(0, 100, teamId)
	if resp.Error != nil {
		return nil, resp.Error
	}

	return hooks, nil
}

func (p *Plugin) processWebhook(wh *model.OutgoingWebhook, post *model.Post, user *model.User, channel *model.Channel) {
	// Проверка, активен ли вебхук
	if wh.CreatorId == "" {
		return
	}

	// Проверка канала, если указан
	if wh.ChannelId != "" && wh.ChannelId != post.ChannelId {
		return
	}

	// Проверка триггерных слов
	triggerMatch := p.checkTriggerWords(wh, post.Message)
	if !triggerMatch {
		return
	}

	// Отправка HTTP-запроса
	response, err := p.sendHTTPRequest(wh, post, user, channel)
	if err != nil {
		p.API.LogError("Failed to send HTTP request",
			"webhook_id", wh.Id,
			"error", err.Error())
		return
	}

	// Обработка ответа
	p.handleResponse(response, post, channel)
}

func (p *Plugin) checkTriggerWords(wh *model.OutgoingWebhook, message string) bool {
	if len(wh.TriggerWords) == 0 {
		return true
	}

	messageLower := strings.ToLower(message)
	for _, triggerWord := range wh.TriggerWords {
		if wh.TriggerWhen == 1 {
			// Проверяем, начинается ли сообщение с триггерного слова
			if strings.HasPrefix(messageLower, strings.ToLower(triggerWord)) {
				return true
			}
		} else if wh.TriggerWhen == 0 {
			// Проверяем, содержится ли триггерное слово в сообщении
			if strings.Contains(messageLower, strings.ToLower(triggerWord)) {
				return true
			}
		} else {
			// По умолчанию проверяем, начинается ли сообщение с триггерного слова
			if strings.HasPrefix(messageLower, strings.ToLower(triggerWord)) {
				return true
			}
		}
	}
	return false
}

func (p *Plugin) sendHTTPRequest(wh *model.OutgoingWebhook, post *model.Post, user *model.User, channel *model.Channel) (*http.Response, error) {
	// Формируем данные для отправки (аналогично стандартным исходящим вебхукам)
	data := map[string]interface{}{
		"user_id":      user.Id,
		"user_name":    user.Username,
		"channel_id":   post.ChannelId,
		"channel_name": channel.Name,
		"team_id":      channel.TeamId,
		"post_id":      post.Id,
		"text":         post.Message,
		"trigger_word": p.getTriggerWord(wh, post.Message),
		"token":        wh.Token,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal JSON data")
	}

	// Определяем тип содержимого
	contentType := "application/x-www-form-urlencoded"
	if wh.ContentType == "application/json" {
		contentType = "application/json"
	}

	// Создаём и отправляем запрос
	client := &http.Client{}
	req, err := http.NewRequest("POST", wh.CallbackURLs[0], strings.NewReader(string(jsonData)))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create HTTP request")
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "Mattermost-Outgoing-Webhook-Plugin/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send HTTP request")
	}

	return resp, nil
}

func (p *Plugin) getTriggerWord(wh *model.OutgoingWebhook, message string) string {
	if len(wh.TriggerWords) == 0 {
		return ""
	}

	messageLower := strings.ToLower(message)
	for _, triggerWord := range wh.TriggerWords {
		if strings.HasPrefix(messageLower, strings.ToLower(triggerWord)) {
			return triggerWord
		}
	}
	return ""
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
