import { describe, it, expect, vi, beforeEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { ArtifactViewer } from './ArtifactViewer';
import { api, ApiError } from '@/api/client';

// jsdom has no object-URL support; stub it.
beforeEach(() => {
  vi.restoreAllMocks();
  (URL as unknown as { createObjectURL: () => string }).createObjectURL = () => 'blob:mock';
  (URL as unknown as { revokeObjectURL: () => void }).revokeObjectURL = () => {};
});

describe('ArtifactViewer', () => {
  it('renders nothing for a non-multimodal (chat) row', () => {
    const { container } = renderWithProviders(<ArtifactViewer eventId="e1" endpointType="chat" />);
    expect(container.textContent).toBe('');
  });

  it('renders an <img> for an image event', async () => {
    vi.spyOn(api, 'getBlob').mockResolvedValue({ blob: new Blob([new Uint8Array([1, 2])], { type: 'image/png' }), filename: null });
    renderWithProviders(<ArtifactViewer eventId="img1" endpointType="image_generation" />);
    await waitFor(() => expect(screen.getByRole('img')).toBeInTheDocument());
    expect(screen.getByRole('img')).toHaveAttribute('src', 'blob:mock');
  });

  it('shows the url-mode notice on a 409', async () => {
    vi.spyOn(api, 'getBlob').mockRejectedValue(new ApiError(409, 'artifact_is_url', 'url', 'conflict'));
    renderWithProviders(<ArtifactViewer eventId="imgu" endpointType="image_generation" />);
    await waitFor(() => expect(screen.getByText(/returned this image as a URL/i)).toBeInTheDocument());
  });

  it('renders nothing on a 404 (no inline artifact)', async () => {
    vi.spyOn(api, 'getBlob').mockRejectedValue(new ApiError(404, 'not_found', 'nope', 'not_found'));
    const { container } = renderWithProviders(<ArtifactViewer eventId="e404" endpointType="tts" />);
    await waitFor(() => expect(container.textContent).toBe(''));
  });
});
