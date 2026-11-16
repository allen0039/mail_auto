package mail

import (
	"context"
	"time"
)

func (service *Service) verifyFlagsOnly(ctx context.Context, operationID string, position int, account Account, password string, item PlanItem) error {
	reader, ok := service.Remote.(RuleStateRemote)
	if !ok {
		return ErrCapability
	}
	fresh, err := reader.RuleState(ctx, account, password, item.Message)
	if err != nil {
		return err
	}
	if err = service.checkSnapshot(account); err != nil {
		return err
	}
	if !fresh.FlagsKnown || !item.Flags.Satisfied(fresh) {
		return blocked("flags_not_confirmed", "只读核验未确认所需标记；未重放 STORE，请在原邮箱检查", ErrUncertain)
	}
	service.invalidateAutomatic(account.ID)
	if err = service.publishFlags(item.Message, fresh); err != nil {
		return err
	}
	_, err = service.Store.DB.Exec("UPDATE operation_items SET state='completed',error_code='',recovery_code='',recovery_detail='',checked_at=? WHERE operation_id=? AND position=? AND state='needs_verification'", time.Now().Unix(), operationID, position)
	return err
}
