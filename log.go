package main

import (
	"os"

	log "github.com/sirupsen/logrus"
)

func initLogger() {
	// Log to stdout and with JSON format; suitable for GKE
	formatter := &log.JSONFormatter{
		FieldMap: log.FieldMap{
			log.FieldKeyTime:  "time",
			log.FieldKeyLevel: "level",
			log.FieldKeyMsg:   "message",
		},
	}

	log.SetOutput(os.Stdout)
	log.SetFormatter(formatter)
}

type deliveryFieldHook struct {
	deliveryID string
}

func newDeliveryFieldHook(deliveryID string) *deliveryFieldHook {
	return &deliveryFieldHook{
		deliveryID: deliveryID,
	}
}

func (h *deliveryFieldHook) Levels() []log.Level {
	return log.AllLevels
}

func (h *deliveryFieldHook) Fire(entry *log.Entry) error {
	entry.Data["delivery"] = h.deliveryID
	return nil
}
