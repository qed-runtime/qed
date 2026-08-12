package agent

import "testing"

func TestValidateContextRebaseEvent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		event   Event
		wantErr bool
	}{
		"valid initial": {
			event: Event{
				ContextCheckpoint: &ContextCheckpoint{Generation: 1, LastRebaseGeneration: 1},
				ContextCompaction: &ContextCompactionReport{
					Applied: true, Rebased: true, RebaseReason: ContextRebaseInitial,
				},
			},
		},
		"valid interval": {
			event: Event{
				ContextCheckpoint: &ContextCheckpoint{Generation: 5, LastRebaseGeneration: 5},
				ContextCompaction: &ContextCompactionReport{
					Applied: true, Rebased: true, RebaseReason: ContextRebaseGenerationInterval,
				},
			},
		},
		"reason without flag": {
			event: Event{
				ContextCompaction: &ContextCompactionReport{RebaseReason: ContextRebaseGenerationInterval},
			},
			wantErr: true,
		},
		"missing checkpoint": {
			event: Event{
				ContextCompaction: &ContextCompactionReport{
					Applied: true, Rebased: true, RebaseReason: ContextRebaseGenerationInterval,
				},
			},
			wantErr: true,
		},
		"wrong generation": {
			event: Event{
				ContextCheckpoint: &ContextCheckpoint{Generation: 5, LastRebaseGeneration: 4},
				ContextCompaction: &ContextCompactionReport{
					Applied: true, Rebased: true, RebaseReason: ContextRebaseGenerationInterval,
				},
			},
			wantErr: true,
		},
		"unknown reason": {
			event: Event{
				ContextCheckpoint: &ContextCheckpoint{Generation: 5, LastRebaseGeneration: 5},
				ContextCompaction: &ContextCompactionReport{
					Applied: true, Rebased: true, RebaseReason: "unknown",
				},
			},
			wantErr: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateContextRebaseEvent(test.event)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateContextRebaseEvent() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
