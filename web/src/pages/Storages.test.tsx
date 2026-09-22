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
