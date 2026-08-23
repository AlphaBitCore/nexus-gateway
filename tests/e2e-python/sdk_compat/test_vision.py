"""AP-3 cases 31-35 — vision (image_url content parts) via the official SDK.

The image is an inline base64 data URI: no fixture file to lose, and no network
fetch that would make the test depend on a third-party host being up.

Proof-of-delivery is a prompt_tokens comparison, not "the model described the
image". A dropped image part would still produce a fluent 200 answer — only the
token count reveals that no pixels reached the provider.
"""

from __future__ import annotations

import pytest

from .conftest import pick_model, skip_if_provider_unreachable

pytestmark = pytest.mark.sdk_compat

PNG_8X8_B64 = (
    "iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAGElEQVR42mNgYPj/n4EBC4ldFCw8"
    "CHUAAOkwP8H8I6eUAAAAAElFTkSuQmCC"
)
PNG_DATA_URI = "data:image/png;base64," + PNG_8X8_B64
QUESTION = "Answer with one short word."


def _image_part(uri: str) -> dict:
    return {"type": "image_url", "image_url": {"url": uri}}


def _prompt_tokens(client, model, content) -> int:
    resp = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": content}],
        max_tokens=16,
    )
    return resp.usage.prompt_tokens


def test_image_url_data_uri_reaches_the_provider(openai_client, catalog) -> None:
    """An image part must raise prompt_tokens above the text-only baseline.

    This is the assertion that a silently-dropped image cannot pass.
    """
    model = pick_model(catalog, feature="vision", family="gpt-4o")
    with_image = _prompt_tokens(
        openai_client, model, [{"type": "text", "text": QUESTION}, _image_part(PNG_DATA_URI)]
    )
    text_only = _prompt_tokens(openai_client, model, [{"type": "text", "text": QUESTION}])
    assert with_image > text_only, (
        f"prompt_tokens with image ({with_image}) must exceed text-only ({text_only}); "
        f"equal counts mean the image part never reached the provider"
    )


def test_image_url_cross_format(openai_client, catalog) -> None:
    """OpenAI `image_url` must convert to Anthropic's source:{type,media_type,data}."""
    model = pick_model(catalog, feature="vision", family="claude-")
    with_image = _prompt_tokens(
        openai_client, model, [{"type": "text", "text": QUESTION}, _image_part(PNG_DATA_URI)]
    )
    text_only = _prompt_tokens(openai_client, model, [{"type": "text", "text": QUESTION}])
    assert with_image > text_only, (
        f"cross-format image dropped: prompt_tokens {with_image} vs text-only {text_only}"
    )


def test_unsupported_image_media_type_is_client_error(openai_client, catalog) -> None:
    """An unsupported media type must be a clean 4xx, never a 5xx.

    A 5xx here would blame the gateway for what is a caller mistake, and would
    pollute provider-availability metrics.
    """
    import openai

    model = pick_model(catalog, feature="vision", family="claude-")
    with pytest.raises(openai.APIStatusError) as excinfo:
        openai_client.chat.completions.create(
            model=model,
            messages=[
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": QUESTION},
                        _image_part("data:image/tiff;base64," + PNG_8X8_B64),
                    ],
                }
            ],
            max_tokens=16,
        )
    assert 400 <= excinfo.value.status_code < 500, (
        f"unsupported media type must be a client error, got {excinfo.value.status_code}"
    )
    assert str(excinfo.value), "the error must carry a message"


def test_image_to_non_vision_model_fails_cleanly(openai_client, catalog) -> None:
    """A model without the vision capability must reject the image, not 5xx."""
    import openai

    model = pick_model(catalog, lacks="vision")
    with pytest.raises(openai.APIStatusError) as excinfo:
        openai_client.chat.completions.create(
            model=model,
            messages=[
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": QUESTION},
                        _image_part(PNG_DATA_URI),
                    ],
                }
            ],
            max_tokens=16,
        )
    skip_if_provider_unreachable(excinfo.value, model)
    assert 400 <= excinfo.value.status_code < 500, (
        f"a non-vision model must fail with a client error, got {excinfo.value.status_code}"
    )


def test_two_images_in_one_turn_accepted(openai_client, catalog) -> None:
    """Multiple image parts in one message must all be carried.

    Two images cost more prompt tokens than one; an ordering or overwrite bug in
    the content-array mapping shows up as the counts matching.
    """
    model = pick_model(catalog, feature="vision", family="gpt-4o")
    two = _prompt_tokens(
        openai_client,
        model,
        [
            {"type": "text", "text": QUESTION},
            _image_part(PNG_DATA_URI),
            _image_part(PNG_DATA_URI),
        ],
    )
    one = _prompt_tokens(
        openai_client, model, [{"type": "text", "text": QUESTION}, _image_part(PNG_DATA_URI)]
    )
    assert two > one, (
        f"two images ({two} prompt tokens) must cost more than one ({one}); "
        f"equal counts mean the second part was dropped"
    )
