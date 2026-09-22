import { render, screen } from '@testing-library/react'
import { beforeEach, expect, test } from 'vitest'
import App from './App'

beforeEach(() => localStorage.clear())

test('renders the app (login when unauthenticated)', () => {
  render(<App />)
  // Unauthenticated: the login screen is shown, branded "Duskrun".
  expect(screen.getByText('Duskrun')).toBeInTheDocument()
  expect(screen.getByLabelText('Email')).toBeInTheDocument()
})
