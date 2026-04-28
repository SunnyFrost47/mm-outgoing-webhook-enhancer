package main

import (
	"sync"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
)

// Плагин реализует интерфейс, ожидаемый сервером Mattermost для связи между сервером и процессами плагина.
type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock синхронизирует доступ к конфигурации.
	configurationLock sync.RWMutex

	// configuration это активная конфигурация плагина. Для получения информации об использовании см. getConfiguration и setConfiguration.
	configuration *configuration

	router    *mux.Router
	botUserID string
}

// OnActivate это метод, вызываемый сервером Mattermost после активации плагина.
func (p *Plugin) OnActivate() error {
	// Принудительно загружаем конфигурацию.
	if err := p.OnConfigurationChange(); err != nil {
		return err
	}

	// Создаём (или обновляем) бота с нужными параметрами
	bot := &model.Bot{
		Username:    "outgoing-webhook",            // Уникальное имя пользователя-бота
		DisplayName: "Outgoing Webhook Plugin Bot", // Отображаемое имя
		Description: "Бот для отправки ответов исходящих вебхуков.",
	}

	botUserID, appErr := p.Helpers.EnsureBot(bot)
	if appErr != nil {
		p.API.LogError("Failed to ensure bot user", "error", appErr.Error())
		return errors.Wrap(appErr, "failed to ensure bot user")
	}

	// Сохраняем ID бота для дальнейшего использования
	p.botUserID = botUserID

	p.initializeAPI()

	p.API.LogInfo("Plugin activated", "bot_user_id", p.botUserID)
	return nil
}

func (p *Plugin) OnDeactivate() error {
	p.router = nil
	p.botUserID = ""

	return nil
}

// See https://developers.mattermost.com/extend/plugins/server/reference/
