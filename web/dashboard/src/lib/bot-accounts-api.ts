
export interface BotPlatformAccount {
  id: number
  platform: string
  account_id: string
  disabled: boolean
  first_seen_at: string
  last_seen_at: string
  disabled_at: string | null
  last_source: 'ready' | 'inbound' | string
  last_adapter_id: string | null
  last_plugin_id: string | null
  last_gateway_name: string | null
  online: boolean
}

export async function getDiscoveredBotAccounts(): Promise<BotPlatformAccount[]> {
  // Go 后端为单账号 QQ 官方渠道，无多账号发现端点
  return []
}

export async function setDiscoveredBotAccountDisabled(
  _accountId: number,
  _disabled: boolean
): Promise<void> {
  // Go 后端无多账号体系：空实现
}


export async function deleteDiscoveredBotAccount(_accountId: number): Promise<void> {
  // Go 后端无多账号体系：空实现
}
