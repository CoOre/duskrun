// WebAuthn glue between the server's JSON options and navigator.credentials.
//
// The server (go-webauthn) speaks the WebAuthn JSON form: every binary field is
// base64url without padding. The browser API wants ArrayBuffers in and hands
// ArrayBuffers back, so this file converts both ways. It is written by hand
// rather than through PublicKeyCredential.parseCreationOptionsFromJSON because
// that helper is still missing from browsers people run, and a dependency for
// forty lines of base64 is not worth carrying.

import type { PublicKeyOptionsJSON } from './api/types'

export function base64urlToBuffer(s: string): ArrayBuffer {
  const pad = s.length % 4 === 0 ? '' : '='.repeat(4 - (s.length % 4))
  const bin = atob(s.replace(/-/g, '+').replace(/_/g, '/') + pad)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out.buffer
}

export function bufferToBase64url(buf: ArrayBuffer | ArrayBufferView): string {
  const bytes = buf instanceof ArrayBuffer ? new Uint8Array(buf) : new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength)
  let bin = ''
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i])
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

type Descriptor = { id: string; type: string; transports?: string[] }

function descriptors(list: unknown): PublicKeyCredentialDescriptor[] | undefined {
  if (!Array.isArray(list)) return undefined
  return (list as Descriptor[]).map((d) => ({
    ...d,
    id: base64urlToBuffer(d.id),
  })) as PublicKeyCredentialDescriptor[]
}

// toCreationOptions decodes the binary fields of a registration request:
// challenge, user.id and excludeCredentials[].id. Everything else (rp, params,
// authenticatorSelection, extensions) passes through untouched.
export function toCreationOptions(json: PublicKeyOptionsJSON): PublicKeyCredentialCreationOptions {
  const pk = json.publicKey as Record<string, any>
  return {
    ...pk,
    challenge: base64urlToBuffer(pk.challenge),
    user: { ...pk.user, id: base64urlToBuffer(pk.user.id) },
    excludeCredentials: descriptors(pk.excludeCredentials),
  } as PublicKeyCredentialCreationOptions
}

// toRequestOptions decodes the binary fields of an assertion request.
export function toRequestOptions(json: PublicKeyOptionsJSON): PublicKeyCredentialRequestOptions {
  const pk = json.publicKey as Record<string, any>
  return {
    ...pk,
    challenge: base64urlToBuffer(pk.challenge),
    allowCredentials: descriptors(pk.allowCredentials),
  } as PublicKeyCredentialRequestOptions
}

// serializeCredential turns a PublicKeyCredential into the JSON go-webauthn
// parses: registration and assertion responses differ only in which fields the
// response carries, so one function covers both.
export function serializeCredential(cred: PublicKeyCredential): Record<string, unknown> {
  const r = cred.response as AuthenticatorResponse & Record<string, any>
  const response: Record<string, unknown> = { clientDataJSON: bufferToBase64url(r.clientDataJSON) }
  if (r.attestationObject) {
    response.attestationObject = bufferToBase64url(r.attestationObject)
    if (typeof r.getTransports === 'function') response.transports = r.getTransports()
  }
  if (r.authenticatorData) response.authenticatorData = bufferToBase64url(r.authenticatorData)
  if (r.signature) response.signature = bufferToBase64url(r.signature)
  if (r.userHandle) response.userHandle = bufferToBase64url(r.userHandle)

  const out: Record<string, unknown> = {
    id: cred.id,
    rawId: bufferToBase64url(cred.rawId),
    type: cred.type,
    response,
    clientExtensionResults: typeof cred.getClientExtensionResults === 'function' ? cred.getClientExtensionResults() : {},
  }
  const attachment = (cred as PublicKeyCredential & { authenticatorAttachment?: string | null }).authenticatorAttachment
  if (attachment) out.authenticatorAttachment = attachment
  return out
}

// webauthnSupported reports whether this browser context can use passkeys at
// all. Outside a secure context (plain http on anything but localhost) the API
// is simply absent.
export function webauthnSupported(): boolean {
  return typeof window !== 'undefined' && typeof window.PublicKeyCredential !== 'undefined' && !!navigator.credentials
}

export async function createPasskey(json: PublicKeyOptionsJSON): Promise<Record<string, unknown>> {
  const cred = (await navigator.credentials.create({ publicKey: toCreationOptions(json) })) as PublicKeyCredential | null
  if (!cred) throw new Error('браузер не вернул ключ')
  return serializeCredential(cred)
}

export async function getPasskeyAssertion(json: PublicKeyOptionsJSON): Promise<Record<string, unknown>> {
  const cred = (await navigator.credentials.get({ publicKey: toRequestOptions(json) })) as PublicKeyCredential | null
  if (!cred) throw new Error('браузер не вернул ключ')
  return serializeCredential(cred)
}

// webauthnErrorMessage explains a browser-side failure. The common one is
// NotAllowedError, which covers both "the user cancelled" and "the prompt timed
// out" — the browser deliberately does not say which.
export function webauthnErrorMessage(e: unknown): string {
  const name = e instanceof Error || (e && typeof e === 'object' && 'name' in e) ? String((e as { name: unknown }).name) : ''
  switch (name) {
    case 'NotAllowedError':
      return 'Операция отменена или время ожидания истекло'
    case 'InvalidStateError':
      return 'Этот ключ уже зарегистрирован'
    case 'SecurityError':
      return 'Браузер отклонил запрос: адрес страницы не совпадает с DUSKRUN_PUBLIC_URL или открыт не по HTTPS'
    case 'NotSupportedError':
      return 'Браузер или ключ не поддерживает нужный алгоритм'
    default:
      return e instanceof Error ? e.message : String(e)
  }
}
