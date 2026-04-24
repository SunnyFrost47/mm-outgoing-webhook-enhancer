package main

import (
	"sync"

	"github.com/gorilla/mux"

	"github.com/mattermost/mattermost-server/v5/plugin"
)

// Плагин реализует интерфейс, ожидаемый сервером Mattermost для связи между сервером и процессами плагина.
type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock синхронизирует доступ к конфигурации.
	configurationLock sync.RWMutex

	// configuration это активная конфигурация плагина. Для получения информации об использовании см. getConfiguration и setConfiguration.
	configuration *configuration

	router *mux.Router
}

// OnActivate это метод, вызываемый сервером Mattermost после активации плагина.
func (p *Plugin) OnActivate() error {
	// Принудительно загружаем конфигурацию.
	if err := p.OnConfigurationChange(); err != nil {
		return err
	}

	p.initializeAPI()

	return nil
}

func (p *Plugin) OnDeactivate() error {

	return nil
}

// See https://developers.mattermost.com/extend/plugins/server/reference/
