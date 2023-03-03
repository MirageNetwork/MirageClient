package miramenu

type dataEventHandlerInfo struct {
	handler DataEventHandle
	once    bool
}

type DataEventHandle func(data interface{})

type DataEvent struct {
	handlers []dataEventHandlerInfo
}

func (e *DataEvent) Attach(handler DataEventHandle) int {
	handlerInfo := dataEventHandlerInfo{handler, false}

	for i, h := range e.handlers {
		if h.handler == nil {
			e.handlers[i] = handlerInfo
			return i
		}
	}

	e.handlers = append(e.handlers, handlerInfo)

	return len(e.handlers) - 1
}

func (e *DataEvent) Detach(handle int) {
	e.handlers[handle].handler = nil
}

func (e *DataEvent) Once(handler DataEventHandle) {
	i := e.Attach(handler)
	e.handlers[i].once = true
}

type DataEventPublisher struct {
	event DataEvent
}

func (p *DataEventPublisher) Event() *DataEvent {
	return &p.event
}

func (p *DataEventPublisher) Publish(data interface{}) {
	for i, h := range p.event.handlers {
		if h.handler != nil {
			h.handler(data)

			if h.once {
				p.event.Detach(i)
			}
		}
	}
}
