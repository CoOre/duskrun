import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import Storages from './Storages'

beforeEach(() => localStorage.clear())

test('TestCreateStoragePostsPayload: submitting the modal posts the right JSON', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/storages': [],
    'POST /api/storages': { id: 7 },
  })
  renderWithAuth(<Storages />, { fetchImpl })

  expect(await screen.findByText('Хранилищ пока нет.')).toBeInTheDocument()

  // Open the modal, fill it, and pick the S3 plugin card.
  await user.click(screen.getByRole('button', { name: /Хранилище/ }))
  await user.type(screen.getByLabelText('Название'), 'offsite')
  await user.click(screen.getByText('S3'))
  await user.type(screen.getByLabelText('Bucket'), 'backups')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = fetchImpl
    .calls()
    .find(([url, init]) => url === '/api/storages' && init?.method === 'POST')
  expect(post).toBeDefined()
  const body = JSON.parse(post![1].body as string)
  expect(body).toMatchObject({ name: 'offsite', type: 's3', config: { bucket: 'backups' } })
})

test('TestStorageClickOpensEditor: clicking a storage opens edit modal', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/storages': [{ id: 1, name: 'local', type: 'localfs', config: { root: '/backups' }, secret_ref: '' }],
  })
  renderWithAuth(<Storages />, { fetchImpl })

  await user.click(await screen.findByText('local'))

  expect(await screen.findByText('Редактировать хранилище')).toBeInTheDocument()
  expect(screen.getByDisplayValue('local')).toBeInTheDocument()
  expect(screen.getByLabelText('Путь')).toHaveValue('/backups')
})

test('TestCreateSftpStorage: the modal posts host/user/path and the key as private_key_ref', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/storages': [],
    'GET /api/secrets': [{ id: 4, name: 'ssh/offsite', type: 'ssh-key', created_at: '2026-08-01T00:00:00Z' }],
    'POST /api/storages': { id: 3 },
  })
  renderWithAuth(<Storages />, { fetchImpl })

  expect(await screen.findByText('Хранилищ пока нет.')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: /Хранилище/ }))
  await user.click(screen.getByText('SFTP'))
  await user.type(screen.getByLabelText('Название'), 'offsite')
  await user.type(screen.getByLabelText('Хост'), 'nas.corp.io')
  await user.type(screen.getByLabelText('Пользователь'), 'backup')
  await user.clear(screen.getByLabelText('Путь'))
  await user.type(screen.getByLabelText('Путь'), '/srv/backup')
  await user.selectOptions(screen.getByLabelText('SSH-ключ'), 'secret://ssh/offsite')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/storages' && init?.method === 'POST')
  expect(post).toBeDefined()
  expect(JSON.parse(post![1].body as string)).toMatchObject({
    name: 'offsite',
    type: 'sftp',
    config: {
      host: 'nas.corp.io',
      port: 22,
      user: 'backup',
      path: '/srv/backup',
      host_key_mode: 'tofu',
      private_key_ref: 'secret://ssh/offsite',
    },
  })
})

test('TestCreatePrivateS3Storage: the modal posts both key refs and path-style', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/storages': [],
    'GET /api/secrets': [
      { id: 1, name: 's3/access', type: 's3-key', created_at: '2026-08-01T00:00:00Z' },
      { id: 2, name: 's3/secret', type: 's3-key', created_at: '2026-08-01T00:00:00Z' },
    ],
    'POST /api/storages': { id: 9 },
  })
  renderWithAuth(<Storages />, { fetchImpl })

  expect(await screen.findByText('Хранилищ пока нет.')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: /Хранилище/ }))
  await user.click(screen.getByText('S3'))
  await user.type(screen.getByLabelText('Название'), 'minio')
  await user.type(screen.getByLabelText('Bucket'), 'backups')
  await user.type(screen.getByLabelText('Endpoint (опц.)'), 'https://minio.corp.io')
  await user.selectOptions(await screen.findByLabelText('Access key'), 'secret://s3/access')
  await user.selectOptions(screen.getByLabelText('Secret key'), 'secret://s3/secret')
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/storages' && init?.method === 'POST')
  expect(post).toBeDefined()
  expect(JSON.parse(post![1].body as string)).toMatchObject({
    name: 'minio',
    type: 's3',
    config: {
      bucket: 'backups',
      endpoint: 'https://minio.corp.io',
      force_path_style: true,
      access_key_ref: 'secret://s3/access',
      secret_key_ref: 'secret://s3/secret',
    },
  })
})

test('TestS3LoneKeyRejected: one key ref without the other does not post', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/storages': [],
    'GET /api/secrets': [{ id: 1, name: 's3/access', type: 's3-key', created_at: '2026-08-01T00:00:00Z' }],
    'POST /api/storages': { id: 9 },
  })
  renderWithAuth(<Storages />, { fetchImpl })

  expect(await screen.findByText('Хранилищ пока нет.')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: /Хранилище/ }))
  await user.click(screen.getByText('S3'))
  await user.type(screen.getByLabelText('Название'), 'minio')
  await user.type(screen.getByLabelText('Bucket'), 'backups')
  await user.selectOptions(await screen.findByLabelText('Access key'), 'secret://s3/access')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  expect(await screen.findByRole('alert')).toHaveTextContent('Access key и Secret key указываются вместе')
  expect(fetchImpl.calls().some(([url, init]) => url === '/api/storages' && init?.method === 'POST')).toBe(false)
})

test('TestEditS3StorageKeepsKeyRefs: the edit form loads both refs from the config', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/storages': [{
      id: 1,
      name: 'minio',
      type: 's3',
      config: { bucket: 'backups', access_key_ref: 'secret://s3/access', secret_key_ref: 'secret://s3/secret', force_path_style: true },
      secret_ref: 'secret://s3/access',
    }],
    'GET /api/secrets': [
      { id: 1, name: 's3/access', type: 's3-key', created_at: '2026-08-01T00:00:00Z' },
      { id: 2, name: 's3/secret', type: 's3-key', created_at: '2026-08-01T00:00:00Z' },
    ],
  })
  renderWithAuth(<Storages />, { fetchImpl })

  await user.click(await screen.findByText('minio'))

  expect(await screen.findByLabelText('Access key')).toHaveValue('secret://s3/access')
  expect(screen.getByLabelText('Secret key')).toHaveValue('secret://s3/secret')
  expect(screen.getByRole('checkbox')).toBeChecked()
})
