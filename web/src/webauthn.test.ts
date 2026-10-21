import { expect, test } from 'vitest'
import {
  base64urlToBuffer,
  bufferToBase64url,
  serializeCredential,
  toCreationOptions,
  toRequestOptions,
  webauthnErrorMessage,
} from './webauthn'

const bytes = (b: ArrayBuffer) => Array.from(new Uint8Array(b))

test('base64url round-trips every byte value without padding', () => {
  const all = new Uint8Array(256).map((_, i) => i)
  const enc = bufferToBase64url(all.buffer)
  expect(enc).not.toMatch(/[+/=]/)
  expect(bytes(base64urlToBuffer(enc))).toEqual(Array.from(all))
})

test('base64url decodes lengths that need padding', () => {
  // "AQ" is one byte, "AQI" two: both are lengths base64 pads.
  expect(bytes(base64urlToBuffer('AQ'))).toEqual([1])
  expect(bytes(base64urlToBuffer('AQI'))).toEqual([1, 2])
  expect(bytes(base64urlToBuffer('-_8'))).toEqual([0xfb, 0xff])
})

test('bufferToBase64url honours a view\'s offset', () => {
  const backing = new Uint8Array([9, 9, 1, 2, 9])
  expect(bufferToBase64url(backing.subarray(2, 4))).toBe('AQI')
})

test('toCreationOptions decodes challenge, user.id and excluded ids only', () => {
  const opts = toCreationOptions({
    publicKey: {
      challenge: 'AQID',
      rp: { id: 'backup.example.com', name: 'Duskrun' },
      user: { id: 'BAU', name: 'a@corp.io', displayName: 'A' },
      pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
      excludeCredentials: [{ type: 'public-key', id: 'Bg', transports: ['usb'] }],
      authenticatorSelection: { userVerification: 'preferred' },
    },
  })
  expect(bytes(opts.challenge as ArrayBuffer)).toEqual([1, 2, 3])
  expect(bytes(opts.user.id as ArrayBuffer)).toEqual([4, 5])
  expect(opts.user.name).toBe('a@corp.io')
  expect(opts.rp.id).toBe('backup.example.com')
  const ex = opts.excludeCredentials![0]
  expect(bytes(ex.id as ArrayBuffer)).toEqual([6])
  expect(ex.transports).toEqual(['usb'])
  expect(opts.authenticatorSelection?.userVerification).toBe('preferred')
})

test('toRequestOptions decodes the challenge and allowCredentials ids', () => {
  const opts = toRequestOptions({
    publicKey: {
      challenge: 'AQID',
      rpId: 'backup.example.com',
      allowCredentials: [{ type: 'public-key', id: 'Bw' }],
      userVerification: 'preferred',
    },
  })
  expect(bytes(opts.challenge as ArrayBuffer)).toEqual([1, 2, 3])
  expect(bytes(opts.allowCredentials![0].id as ArrayBuffer)).toEqual([7])
  expect(opts.rpId).toBe('backup.example.com')
})

test('toRequestOptions tolerates a missing allow list', () => {
  const opts = toRequestOptions({ publicKey: { challenge: 'AQ' } })
  expect(opts.allowCredentials).toBeUndefined()
})

const buf = (...b: number[]) => new Uint8Array(b).buffer

test('serializeCredential encodes an assertion response', () => {
  const cred = {
    id: 'Bw',
    rawId: buf(7),
    type: 'public-key',
    authenticatorAttachment: 'cross-platform',
    getClientExtensionResults: () => ({}),
    response: {
      clientDataJSON: buf(1),
      authenticatorData: buf(2),
      signature: buf(3),
      userHandle: null,
    },
  } as unknown as PublicKeyCredential
  expect(serializeCredential(cred)).toEqual({
    id: 'Bw',
    rawId: 'Bw',
    type: 'public-key',
    authenticatorAttachment: 'cross-platform',
    clientExtensionResults: {},
    response: { clientDataJSON: 'AQ', authenticatorData: 'Ag', signature: 'Aw' },
  })
})

test('serializeCredential encodes a registration response with transports', () => {
  const cred = {
    id: 'CA',
    rawId: buf(8),
    type: 'public-key',
    authenticatorAttachment: null,
    getClientExtensionResults: () => ({ credProps: { rk: true } }),
    response: {
      clientDataJSON: buf(1),
      attestationObject: buf(4),
      getTransports: () => ['internal', 'hybrid'],
    },
  } as unknown as PublicKeyCredential
  const out = serializeCredential(cred)
  expect(out.response).toEqual({ clientDataJSON: 'AQ', attestationObject: 'BA', transports: ['internal', 'hybrid'] })
  expect(out.clientExtensionResults).toEqual({ credProps: { rk: true } })
  expect(out).not.toHaveProperty('authenticatorAttachment')
})

test('webauthnErrorMessage explains a cancelled prompt', () => {
  const e = new Error('The operation either timed out or was not allowed.')
  e.name = 'NotAllowedError'
  expect(webauthnErrorMessage(e)).toMatch(/отменена/)
})
