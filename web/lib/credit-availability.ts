import type {Account} from './types';

export interface CreditSplit {
  available: number;
  unavailable: number;
  availableRows: Account[];
  unavailableRows: Account[];
}

/**
 * 拆分可用与不可用账号余额。冷却只是临时出池，余额仍可用；自动封禁、
 * 手动停用和状态未知才计入不可用。未同步到余额的账号不参与计算。
 */
export function splitAccountCredits(
  accounts: Account[],
  creditsOf: (account: Account) => number | undefined,
): CreditSplit {
  const availableRows: Account[] = [];
  const unavailableRows: Account[] = [];
  for (const account of accounts) {
    if (typeof creditsOf(account) !== 'number') continue;
    if (account.disabled || account.manual_disabled || account.pool_unknown) {
      unavailableRows.push(account);
    } else {
      availableRows.push(account);
    }
  }
  const sum = (rows: Account[]) =>
    rows.reduce((total, account) => total + (creditsOf(account) || 0), 0);
  return {
    available: sum(availableRows),
    unavailable: sum(unavailableRows),
    availableRows,
    unavailableRows,
  };
}
