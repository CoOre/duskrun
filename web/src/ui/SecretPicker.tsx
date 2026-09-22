import { useState } from 'react'
import type { Secret } from '../api/types'
import { useApiData } from '../api/useApiData'
import { useAuth } from '../auth'
import { inputStyle, labelStyle } from './dc'
import { SecretModal } from './SecretModal'

const NEW_SECRET = '__new_secret__'

function refName(ref: string): string {
  return ref.replace(/^secret:\/\//, '').trim()
}

export function SecretPicker({
  id,
  label,
  value,
  onChange,
  types,
  newSecretType,
}: {
  id: string
  label: string
  value: string
  onChange: (ref: string) => void
  types?: string[]
  newSecretType?: string
}) {
  const { api } = useAuth()
  const [reload, setReload] = useState(0)
  const [creating, setCreating] = useState(false)
  const { data, loading } = useApiData<Secret[]>(() => api.listSecrets(), [reload])
  const secrets = (data ?? []).filter((secret) => !types || types.includes(secret.type))
  const containsCurrent = !value || secrets.some((secret) => `secret://${secret.name}` === value)

  return (
    <>
      <label style={labelStyle} htmlFor={id}>{label}</label>
      <select
        id={id}
        className="dc-select"
        style={inputStyle}
        value={value}
        onChange={(e) => {
          if (e.target.value === NEW_SECRET) {
            setCreating(true)
            return
          }
          onChange(e.target.value)
        }}
      >
        <option value="">Не выбран</option>
        {!containsCurrent && <option value={value}>{refName(value)} (недоступен)</option>}
        {secrets.map((secret) => <option key={secret.id} value={`secret://${secret.name}`}>{secret.name} · {secret.type}</option>)}
        <option value={NEW_SECRET}>{loading ? 'Загрузка секретов…' : '＋ Новый секрет…'}</option>
      </select>
      {creating && (
        <SecretModal
          initialType={newSecretType}
          onClose={() => setCreating(false)}
          onSaved={(name) => {
            setCreating(false)
            setReload((n) => n + 1)
            onChange(`secret://${name}`)
          }}
        />
      )}
    </>
  )
}
