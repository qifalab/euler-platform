package storage

func (m *Module) Close() error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	return nil
}
